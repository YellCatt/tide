package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

func (s *Service) initLog() {
	if err := os.MkdirAll(s.Cfg.PluginDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "创建插件目录失败 [%s]: %v\n", s.Cfg.Name, err)
	}
	if err := os.MkdirAll(filepath.Dir(s.Cfg.LogFilePath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "创建日志目录失败 [%s]: %v\n", s.Cfg.Name, err)
	}
	f, err := os.OpenFile(s.Cfg.LogFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开日志文件失败 [%s]: %v，日志将输出到标准错误\n", s.Cfg.Name, err)
		return
	}
	s.logFile = f
}

func (s *Service) closeLog() {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.logFile != nil {
		_ = s.logFile.Close()
		s.logFile = nil
	}
}

type serviceLogWriter struct{ s *Service }

func (w serviceLogWriter) Write(p []byte) (int, error) {
	w.s.logMu.Lock()
	defer w.s.logMu.Unlock()
	if w.s.logFile != nil {
		return w.s.logFile.Write(p)
	}
	return os.Stderr.Write(p)
}

func (s *Service) childLogWriter() io.Writer { return serviceLogWriter{s: s} }

func (s *Service) logRaw(msg string) {
	line := fmt.Sprintf("[%s] [%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), s.Cfg.Name, msg)
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.logFile != nil {
		_, _ = s.logFile.WriteString(line)
		return
	}
	_, _ = os.Stderr.WriteString(line)
}

func (s *Service) logRawf(level, msg string) { s.logRaw("[" + level + "] " + msg) }
func (s *Service) logInfo(sth string)  { s.logRawf("INFO", sth) }
func (s *Service) logOK(sth string)    { s.logRawf("DEBUG", sth) }
func (s *Service) logWarn(sth string)  { s.logRawf("WARN", sth) }
func (s *Service) logError(sth string) { s.logRawf("ERROR", sth) }
func (s *Service) logStep(sth string)  { s.logRawf("DEBUG", sth) }