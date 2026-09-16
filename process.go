package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ============ 子进程管理 ============
type childProcess struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	done     chan struct{}
	exitCode int
}

var child = &childProcess{}

// startProgram 启动 glean 二进制，stdout/stderr 追加到日志文件。
func startProgram() error {
	if _, err := os.Stat(binaryPath); err != nil {
		logError("二进制文件不存在，无法启动")
		return err
	}

	cmd := exec.Command(binaryPath)
	cmd.Dir = pluginDir
	cmd.Stdout = childLogWriter()
	cmd.Stderr = childLogWriter()

	if err := cmd.Start(); err != nil {
		logError(fmt.Sprintf("程序启动失败: %v", err))
		return err
	}

	done := make(chan struct{})
	child.mu.Lock()
	child.cmd = cmd
	child.done = done
	child.exitCode = -1
	child.mu.Unlock()

	// 回收子进程并记录退出码
	go func() {
		err := cmd.Wait()
		code := exitCodeOf(cmd, err)
		child.mu.Lock()
		child.exitCode = code
		if child.done == done {
			close(done)
		}
		child.mu.Unlock()
	}()

	logOK(fmt.Sprintf("程序已启动 (PID: %d)", cmd.Process.Pid))
	return nil
}

// exitCodeOf 把 shell 语义的退出码算出来（被信号杀死时为 128+信号值）
func exitCodeOf(cmd *exec.Cmd, err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				return 128 + int(ws.Signal())
			}
			return ws.ExitStatus()
		}
		return ee.ExitCode()
	}
	return -1
}

func (c *childProcess) running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd == nil || c.cmd.Process == nil || c.done == nil {
		return false
	}
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

func (c *childProcess) pid() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

func (c *childProcess) lastExitCode() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exitCode
}

func (c *childProcess) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cmd = nil
	c.done = nil
	c.exitCode = -1
}

// stopProgram 先发 SIGTERM 优雅退出，超时后 SIGKILL。
func stopProgram() {
	if !child.running() {
		child.clear()
		return
	}
	pid := child.pid()
	logInfo(fmt.Sprintf("正在停止程序 (PID: %d)...", pid))

	child.mu.Lock()
	done := child.done
	proc := child.cmd.Process
	child.mu.Unlock()

	if proc != nil {
		_ = proc.Signal(syscall.SIGTERM)
	}

	select {
	case <-done:
	case <-time.After(gracefulShutdownTimeout * time.Second):
		logWarn(fmt.Sprintf("程序未在 %d 秒内退出，强制终止", gracefulShutdownTimeout))
		if proc != nil {
			_ = proc.Kill()
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	child.clear()
}

// killProgram 供清理/退出流程使用：直接终止并等待（不打印多余日志）。
func killProgram() {
	if !child.running() {
		return
	}
	child.mu.Lock()
	proc := child.cmd.Process
	done := child.done
	child.mu.Unlock()

	if proc != nil {
		_ = proc.Signal(syscall.SIGTERM)
		select {
		case <-done:
			return
		case <-time.After(time.Second):
		}
		_ = proc.Kill()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
	child.clear()
}

// ============ 残留进程清理 ============
// killLeftoverProcesses 等价于 killall -9 glean，优先扫描 /proc（不依赖 killall 是否存在）。
func killLeftoverProcesses() {
	killed := 0
	entries, err := os.ReadDir("/proc")
	if err == nil {
		self := os.Getpid()
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			pid, err := strconv.Atoi(e.Name())
			if err != nil || pid == self {
				continue
			}
			if readProcComm(pid) != binaryName {
				continue
			}
			if syscall.Kill(pid, syscall.SIGKILL) == nil {
				killed++
			}
		}
	} else if path, err := exec.LookPath("killall"); err == nil {
		if err := exec.Command(path, "-9", binaryName).Run(); err == nil {
			killed++
		}
	}
	if killed > 0 {
		logInfo(fmt.Sprintf("已清理残留的 %s 进程 %d 个", binaryName, killed))
	}
}

func readProcComm(pid int) string {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// ============ PID 文件 ============
func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func readPidFile() (int, bool) {
	data, err := os.ReadFile(pidFilePath)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func writePidFile() error {
	return os.WriteFile(pidFilePath, []byte(strconv.Itoa(os.Getpid())), 0o644)
}
