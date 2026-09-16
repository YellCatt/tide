package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

var (
	appCtx     context.Context
	cancelApp  context.CancelFunc
	cleanupOne sync.Once
	timeLayout = "2006-01-02 15:04:05 MST"
)

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func logGlobal(msg string) {
	line := fmt.Sprintf("[%s] [global] %s\n", time.Now().Format("2006-01-02 15:04:05"), msg)
	_, _ = os.Stderr.WriteString(line)
}

func logGlobalStep(s string)  { logGlobal("【步骤】" + s) }
func logGlobalOK(s string)    { logGlobal("【成功】✓ " + s) }
func logGlobalWarn(s string)  { logGlobal("【警告】⚠ " + s) }
func logGlobalError(s string) { logGlobal("【错误】✗ " + s) }

func (s *Service) mainLoop(ctx context.Context) {
	if err := s.startProgram(); err != nil {
		s.logWarn("本地版本不存在，尝试下载...")
		if !s.downloadBinary(ctx) {
			s.logError("下载失败且无本地版本，无法启动")
			return
		}
		if err := s.applyUpdate(); err != nil {
			s.logError(fmt.Sprintf("替换二进制失败: %v", err))
			return
		}
		if err := s.startProgram(); err != nil {
			s.logError("程序启动失败，守护循环终止")
			return
		}
	}
	s.currentDelay.Store(int64(s.Cfg.RestartDelay))

	s.initUpdateRecord()

	firstCheck := true
	hasChild := true

	for s.running.Load() {
		if s.child.running() {
			if firstCheck {
				s.logStep("程序已启动，立即后台检查新版本...")
				firstCheck = false
				if s.downloadBinary(ctx) {
					if _, err := os.Stat(s.Cfg.BinaryPath); err == nil && fileEquals(s.Cfg.TmpPath, s.Cfg.BinaryPath) {
						s.logInfo("当前已是最新版本，无需替换")
						_ = os.Remove(s.Cfg.TmpPath)
					} else {
						s.logOK("发现新版本，准备热更新")
						s.needUpdate.Store(true)
					}
				} else {
					s.logWarn("启动后更新检查失败，继续使用当前版本")
				}
			} else {
				s.checkAndUpdate(ctx)
			}

			if s.needUpdate.Load() {
				s.logStep("执行热更新...")
				s.stopProgram()
				hasChild = false
				if err := s.applyUpdate(); err != nil {
					s.logError(fmt.Sprintf("替换二进制失败: %v", err))
				} else {
					s.logOK("已替换为新版本")
				}
				s.needUpdate.Store(false)
				if err := s.startProgram(); err != nil {
					s.logError("热更新后启动失败，守护循环终止")
					break
				}
				hasChild = true
				s.currentDelay.Store(int64(s.Cfg.RestartDelay))
			} else if !sleepCtx(ctx, s.Cfg.LoopIdleInterval) {
				break
			}
			continue
		}

		if hasChild {
			exitCode := s.child.lastExitCode()
			s.logRaw("========================================")
			s.logInfo(fmt.Sprintf("程序已退出，退出码: %d", exitCode))
			switch {
			case exitCode == 0:
				s.logInfo("状态: 正常退出")
			case exitCode == 143 || exitCode == 130:
				s.logInfo("状态: 被信号终止（守护脚本主动停止或热更新，属正常）")
			default:
				s.logError("状态: 异常退出")
			}
			s.child.clear()
			hasChild = false
		}

		s.checkAndUpdate(ctx)
		if s.needUpdate.Load() {
			if err := s.applyUpdate(); err != nil {
				s.logError(fmt.Sprintf("替换二进制失败: %v", err))
			} else {
				s.logOK("已更新到新版本")
			}
			s.needUpdate.Store(false)
		}

		delay := s.currentDelay.Load()
		s.logInfo(fmt.Sprintf("等待 %d 秒后重启...", delay))
		if !sleepCtx(ctx, time.Duration(delay)*time.Second) {
			break
		}
		delay *= 2
		if delay > int64(s.Cfg.MaxRestartDelay) {
			delay = int64(s.Cfg.MaxRestartDelay)
		}
		s.currentDelay.Store(delay)

		if err := s.startProgram(); err != nil {
			s.logError("重启失败，守护循环终止")
			break
		}
		hasChild = true
		s.currentDelay.Store(int64(s.Cfg.RestartDelay))
	}
}

func (s *Service) fatalExit(msg string) {
	s.logError(msg)
	_ = os.Remove(s.Cfg.PidFilePath)
	_ = os.Remove(s.Cfg.TmpPath)
	s.closeLog()
}

func main() {
	configPath := "config.yaml"
	if len(os.Args) >= 2 {
		configPath = os.Args[1]
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败 (%s): %v\n", configPath, err)
		os.Exit(1)
	}

	appCtx, cancelApp = context.WithCancel(context.Background())
	defer cancelApp()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigCh
		logGlobal(fmt.Sprintf("收到退出信号 (%v)，开始清理所有服务...", s))
		cleanupOne.Do(func() {
			cancelApp()
		})
	}()

	logGlobalStep("加载配置完成")
	logGlobal(fmt.Sprintf("配置文件: %s", configPath))
	logGlobal(fmt.Sprintf("定义服务数: %d", len(cfg.Services)))

	if len(cfg.Services) == 0 {
		logGlobalError("services 列表为空，没有需要守护的服务")
		os.Exit(1)
	}

	configs := make([]ServiceConfig, 0, len(cfg.Services))
	for _, svcYAML := range cfg.Services {
		svcCfg, err := svcYAML.ToConfig(cfg.Defaults)
		if err != nil {
			logGlobalError(fmt.Sprintf("服务配置错误: %v", err))
			os.Exit(1)
		}
		configs = append(configs, svcCfg)
		logGlobal(fmt.Sprintf("  + %s -> %s", svcCfg.Name, svcCfg.PluginDir))
	}

	waitForNetwork(appCtx)
	logGlobal("========================================")

	services := make([]*Service, 0, len(configs))
	var wg sync.WaitGroup

	for _, svcCfg := range configs {
		svc := NewService(svcCfg)
		services = append(services, svc)
		wg.Add(1)
		go func(s *Service) {
			defer wg.Done()
			defer func() {
				s.killProgram()
				_ = os.Remove(s.Cfg.PidFilePath)
				_ = os.Remove(s.Cfg.TmpPath)
				s.closeLog()
			}()
			s.run(appCtx)
		}(svc)
	}

	wg.Wait()
	logGlobalOK("所有服务已退出，进程结束")
}