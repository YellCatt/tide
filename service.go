package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type childProcess struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	done     chan struct{}
	exitCode int
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

type Service struct {
	Cfg ServiceConfig

	running      atomic.Bool
	needUpdate   atomic.Bool
	currentDelay atomic.Int64

	child *childProcess

	logMu   sync.Mutex
	logFile *os.File

	ctx    context.Context
	cancel context.CancelFunc
}

func NewService(cfg ServiceConfig) *Service {
	s := &Service{
		Cfg:   cfg,
		child: &childProcess{exitCode: -1},
	}
	s.currentDelay.Store(int64(cfg.RestartDelay))
	return s
}

func (s *Service) Name() string { return s.Cfg.Name }

func (s *Service) run(ctx context.Context) {
	s.ctx, s.cancel = context.WithCancel(ctx)
	defer s.cancel()

	s.initLog()
	s.running.Store(true)
	s.currentDelay.Store(int64(s.Cfg.RestartDelay))

	s.logRaw("========================================")
	s.logInfo(fmt.Sprintf("守护脚本启动"))
	if wd, err := os.Getwd(); err == nil {
		s.logInfo(fmt.Sprintf("当前工作目录: %s", wd))
	}
	s.logInfo(fmt.Sprintf("插件目录: %s", s.Cfg.PluginDir))
	s.logInfo(fmt.Sprintf("下载地址: %s", s.Cfg.DownloadURL))
	s.logInfo(fmt.Sprintf("最大下载重试: %d 次", s.Cfg.MaxRetry))
	s.logInfo(fmt.Sprintf("更新检查间隔: %d 秒", s.Cfg.UpdateInterval))
	s.logInfo(fmt.Sprintf("连接超时: %d 秒", int(s.Cfg.ConnectTimeout.Seconds())))
	s.logInfo(fmt.Sprintf("单次下载最大耗时: %d 秒", int(s.Cfg.MaxDownloadTime.Seconds())))

	if pid, ok := s.readPidFile(); ok {
		if s.isProcessAlive(pid) {
			s.fatalExit(fmt.Sprintf("检测到已有实例在运行 (PID: %d)，请勿重复启动", pid))
			return
		}
		s.logWarn("发现残留 PID 文件，但对应进程已不存在，继续启动")
		_ = os.Remove(s.Cfg.PidFilePath)
	}

	s.killLeftoverProcesses()
	_ = os.Remove(s.Cfg.PidFilePath)
	s.logInfo(fmt.Sprintf("已清理可能残留的 %s 进程和 PID 文件", s.Cfg.BinaryName))

	if err := s.writePidFile(); err != nil {
		s.fatalExit(fmt.Sprintf("PID 文件写入失败: %v", err))
		return
	}
	s.logOK(fmt.Sprintf("PID 文件已写入: %s (当前 PID: %d)", s.Cfg.PidFilePath, os.Getpid()))

	s.logRaw("========================================")

	s.logStep("检查插件目录...")
	if st, err := os.Stat(s.Cfg.PluginDir); err != nil || !st.IsDir() {
		s.logInfo(fmt.Sprintf("目录不存在，正在创建: %s", s.Cfg.PluginDir))
		if err := os.MkdirAll(s.Cfg.PluginDir, 0o755); err != nil {
			s.fatalExit("目录创建失败，退出")
			return
		}
		s.logOK("目录创建成功")
	} else {
		s.logOK(fmt.Sprintf("目录已存在: %s", s.Cfg.PluginDir))
	}

	if err := os.Chdir(s.Cfg.PluginDir); err != nil {
		s.fatalExit(fmt.Sprintf("进入目录失败: %s", err))
		return
	}

	s.mainLoop(s.ctx)
}

func (s *Service) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *Service) startProgram() error {
	if _, err := os.Stat(s.Cfg.BinaryPath); err != nil {
		s.logError("二进制文件不存在，无法启动")
		return err
	}

	cmd := exec.Command(s.Cfg.BinaryPath)
	cmd.Dir = s.Cfg.PluginDir
	cmd.Stdout = s.childLogWriter()
	cmd.Stderr = s.childLogWriter()

	if err := cmd.Start(); err != nil {
		s.logError(fmt.Sprintf("程序启动失败: %v", err))
		return err
	}

	done := make(chan struct{})
	s.child.mu.Lock()
	s.child.cmd = cmd
	s.child.done = done
	s.child.exitCode = -1
	s.child.mu.Unlock()

	go func() {
		err := cmd.Wait()
		code := exitCodeOf(cmd, err)
		s.child.mu.Lock()
		s.child.exitCode = code
		if s.child.done == done {
			close(done)
		}
		s.child.mu.Unlock()
	}()

	s.logOK(fmt.Sprintf("程序已启动 (PID: %d)", cmd.Process.Pid))
	return nil
}

func (s *Service) stopProgram() {
	if !s.child.running() {
		s.child.clear()
		return
	}
	pid := s.child.pid()
	s.logInfo(fmt.Sprintf("正在停止程序 (PID: %d)...", pid))

	s.child.mu.Lock()
	done := s.child.done
	proc := s.child.cmd.Process
	s.child.mu.Unlock()

	if proc != nil {
		_ = proc.Signal(syscall.SIGTERM)
	}

	select {
	case <-done:
	case <-time.After(s.Cfg.GracefulShutdownTimeout):
		s.logWarn(fmt.Sprintf("程序未在 %d 秒内退出，强制终止", int(s.Cfg.GracefulShutdownTimeout.Seconds())))
		if proc != nil {
			_ = proc.Kill()
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	s.child.clear()
}

func (s *Service) killProgram() {
	if !s.child.running() {
		return
	}
	s.child.mu.Lock()
	proc := s.child.cmd.Process
	done := s.child.done
	s.child.mu.Unlock()

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
	s.child.clear()
}

func (s *Service) killLeftoverProcesses() {
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
			if readProcComm(pid) != s.Cfg.BinaryName {
				continue
			}
			if killProcess(pid) {
				killed++
			}
		}
	} else if path, err := exec.LookPath("killall"); err == nil {
		if err := exec.Command(path, "-9", s.Cfg.BinaryName).Run(); err == nil {
			killed++
		}
	}
	if killed > 0 {
		s.logInfo(fmt.Sprintf("已清理残留的 %s 进程 %d 个", s.Cfg.BinaryName, killed))
	}
}

func readProcComm(pid int) string {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (s *Service) isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func killProcess(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.SIGKILL) == nil
}

func (s *Service) readPidFile() (int, bool) {
	data, err := os.ReadFile(s.Cfg.PidFilePath)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func (s *Service) writePidFile() error {
	return os.WriteFile(s.Cfg.PidFilePath, []byte(strconv.Itoa(os.Getpid())), 0o644)
}