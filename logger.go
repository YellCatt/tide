package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	logMu     sync.Mutex
	logHandle *os.File
)

// initLog 打开日志文件（追加写）；失败时日志退回标准错误。
// 预创建目录，确保早期日志能写入。
func initLog() {
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "创建插件目录失败: %v\n", err)
	}
	if err := os.MkdirAll(filepath.Dir(logFilePath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "创建日志目录失败: %v\n", err)
	}
	f, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开日志文件失败: %v，日志将输出到标准错误\n", err)
		return
	}
	logHandle = f
}

func closeLog() {
	logMu.Lock()
	defer logMu.Unlock()
	if logHandle != nil {
		_ = logHandle.Close()
		logHandle = nil
	}
}

// logWriter 供子进程 stdout/stderr 复用同一个日志文件（写入加锁）。
type logWriterT struct{}

func (logWriterT) Write(p []byte) (int, error) {
	logMu.Lock()
	defer logMu.Unlock()
	if logHandle != nil {
		return logHandle.Write(p)
	}
	return os.Stderr.Write(p)
}

// childLogWriter 返回子进程可用的日志写入器
func childLogWriter() io.Writer { return logWriterT{} }

// ============ 日志函数 ============
func logRaw(msg string) {
	line := fmt.Sprintf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), msg)
	logMu.Lock()
	defer logMu.Unlock()
	if logHandle != nil {
		_, _ = logHandle.WriteString(line)
		return
	}
	_, _ = os.Stderr.WriteString(line)
}

func logInfo(s string)  { logRaw("【信息】" + s) }
func logOK(s string)    { logRaw("【成功】✓ " + s) }
func logWarn(s string)  { logRaw("【警告】⚠ " + s) }
func logError(s string) { logRaw("【错误】✗ " + s) }
func logStep(s string)  { logRaw("【步骤】" + s) }
