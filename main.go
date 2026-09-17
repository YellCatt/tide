package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
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

const configReloadInterval = 10 * time.Second

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

func logGlobalf(level, s string) { logGlobal("[" + level + "] " + s) }
func logGlobalStep(s string)  { logGlobalf("DEBUG", s) }
func logGlobalOK(s string)    { logGlobalf("INFO", s) }
func logGlobalWarn(s string)  { logGlobalf("WARN", s) }
func logGlobalError(s string) { logGlobalf("ERROR", s) }

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

type serviceEntry struct {
	svc *Service
	wg  *sync.WaitGroup
	cfg ServiceConfig
}

type serviceManager struct {
	mu      sync.Mutex
	entries map[string]*serviceEntry
}

func newServiceManager() *serviceManager {
	return &serviceManager{entries: make(map[string]*serviceEntry)}
}

func (m *serviceManager) startService(svcCfg ServiceConfig) {
	svc := NewService(svcCfg)
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func(s *Service, w *sync.WaitGroup) {
		defer w.Done()
		defer func() {
			s.killProgram()
			_ = os.Remove(s.Cfg.PidFilePath)
			_ = os.Remove(s.Cfg.TmpPath)
			s.closeLog()
		}()
		s.run(appCtx)
	}(svc, wg)

	m.mu.Lock()
	m.entries[svcCfg.Name] = &serviceEntry{svc: svc, wg: wg, cfg: svcCfg}
	m.mu.Unlock()

	logGlobalOK(fmt.Sprintf("服务已启动: %s", svcCfg.Name))
}

func (m *serviceManager) stopService(name string) {
	m.mu.Lock()
	entry, ok := m.entries[name]
	if !ok {
		m.mu.Unlock()
		return
	}
	delete(m.entries, name)
	m.mu.Unlock()

	logGlobal(fmt.Sprintf("正在停止服务: %s", name))
	entry.svc.Stop()
	entry.wg.Wait()
	logGlobalOK(fmt.Sprintf("服务已停止: %s", name))
}

func (m *serviceManager) stopAll() {
	m.mu.Lock()
	names := make([]string, 0, len(m.entries))
	for name := range m.entries {
		names = append(names, name)
	}
	m.mu.Unlock()

	for _, name := range names {
		m.stopService(name)
	}
}

func (m *serviceManager) reload(newCfg GlobalConfig) {
	newMap := make(map[string]ServiceConfig)
	for _, svcYAML := range newCfg.Services {
		svcCfg, err := svcYAML.ToConfig(newCfg.Defaults)
		if err != nil {
			logGlobalError(fmt.Sprintf("配置热重载: 服务 %q 配置错误: %v，跳过", svcYAML.Name, err))
			continue
		}
		if !svcCfg.Enabled {
			logGlobal(fmt.Sprintf("配置热重载: 服务 %s 已禁用，跳过", svcYAML.Name))
			continue
		}
		newMap[svcYAML.Name] = svcCfg
	}

	m.mu.Lock()
	oldNames := make(map[string]bool)
	for name := range m.entries {
		oldNames[name] = true
	}
	newNames := make(map[string]bool)
	for name := range newMap {
		newNames[name] = true
	}
	m.mu.Unlock()

	for name := range oldNames {
		if !newNames[name] {
			logGlobal(fmt.Sprintf("配置变更: 停止服务 %s", name))
			m.stopService(name)
		}
	}

	for name, newSvcCfg := range newMap {
		if oldNames[name] {
			m.mu.Lock()
			entry := m.entries[name]
			m.mu.Unlock()
			if entry != nil && !reflect.DeepEqual(entry.cfg, newSvcCfg) {
				logGlobal(fmt.Sprintf("配置变更: 重启服务 %s（配置已更新）", name))
				m.stopService(name)
				m.startService(newSvcCfg)
			}
		} else {
			logGlobal(fmt.Sprintf("配置变更: 启动服务 %s", name))
			m.startService(newSvcCfg)
		}
	}
}

func configModTime(path string) (time.Time, error) {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return st.ModTime(), nil
}

func watchConfig(ctx context.Context, path string, mgr *serviceManager, initialModTime time.Time) {
	lastMod := initialModTime
	ticker := time.NewTicker(configReloadInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			mod, err := configModTime(path)
			if err != nil {
				logGlobalWarn(fmt.Sprintf("检查配置文件失败: %v", err))
				continue
			}
			if !mod.Equal(lastMod) {
				lastMod = mod
				logGlobal(fmt.Sprintf("检测到配置文件变更 (%s)，重新加载...", path))

				cfg, err := LoadConfig(path)
				if err != nil {
					logGlobalError(fmt.Sprintf("重新加载配置失败: %v", err))
					continue
				}
				mgr.reload(cfg)
				logGlobalOK("配置热重载完成")
			}
		}
	}
}

func main() {
	configPath := "config.yaml"
	if len(os.Args) >= 2 {
		configPath = os.Args[1]
	}
	absConfigPath, _ := filepath.Abs(configPath)

	if err := ensureConfig(configPath); err != nil {
		fmt.Fprintf(os.Stderr, "写入默认配置失败 (%s): %v\n", configPath, err)
		os.Exit(1)
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
	logGlobal(fmt.Sprintf("配置文件: %s", absConfigPath))
	logGlobal(fmt.Sprintf("配置热重载检查间隔: %v", configReloadInterval))

	svcCfgs := make([]ServiceConfig, 0, len(cfg.Services))
	disabledCount := 0
	for _, svcYAML := range cfg.Services {
		svcCfg, err := svcYAML.ToConfig(cfg.Defaults)
		if err != nil {
			logGlobalError(fmt.Sprintf("服务配置错误: %v", err))
			os.Exit(1)
		}
		svcCfgs = append(svcCfgs, svcCfg)
		state := "启用"
		if !svcCfg.Enabled {
			state = "禁用"
			disabledCount++
		}
		logGlobal(fmt.Sprintf("  + %s -> %s [%s]", svcCfg.Name, svcCfg.PluginDir, state))
	}

	enabledCount := len(svcCfgs) - disabledCount
	logGlobal(fmt.Sprintf("定义服务数: %d（启用: %d, 禁用: %d）", len(svcCfgs), enabledCount, disabledCount))

	if enabledCount == 0 {
		logGlobalError("没有任何启用的服务，退出")
		os.Exit(1)
	}

	waitForNetwork(appCtx)
	logGlobal("========================================")

	mgr := newServiceManager()
	for _, sc := range svcCfgs {
		if sc.Enabled {
			mgr.startService(sc)
		}
	}

	go func() {
		mod, _ := configModTime(absConfigPath)
		watchConfig(appCtx, absConfigPath, mgr, mod)
	}()

	<-appCtx.Done()
	logGlobal("正在停止所有服务...")
	mgr.stopAll()
	logGlobalOK("所有服务已退出，进程结束")
}

const defaultConfigYAML = `# tide 守护脚本配置文件
# 时长字段支持 "120s" / "5m" / "2h"，也可以直接写秒数（如 120）

defaults:
  # 全局启用开关（每个服务可单独覆盖为 false 来禁用）
  enabled: true
  # 插件目录（每个服务可单独覆盖）
  plugin_dir: /plugins/data/tide
  # 最大下载重试次数
  max_retry: 20
  # 进程崩溃后的初始重启延迟（秒），每次失败翻倍直到 max_restart_delay
  restart_delay: 5
  # 最大重启延迟（秒）
  max_restart_delay: 300
  # 定时检查更新的间隔（秒）
  update_interval: 14400
  # 优雅退出等待时间（秒），超时后 SIGKILL
  graceful_shutdown_timeout: 10
  # HTTP 连接超时
  connect_timeout: "120s"
  # 单次下载最大耗时
  max_download_time: "1200s"
  # 网络就绪检测轮询间隔
  network_check_interval: "5s"
  # 下载重试之间的等待时间
  download_retry_delay: "10s"
  # 守护循环空闲轮询间隔
  loop_idle_interval: "10s"

services:
  - name: glean
    enabled: true
    plugin_dir: /plugins/data/glean
    download_url: https://github.com/YellCatt/glean/releases/download/dev-latest/default.glean_linux_mipsle

  # 多服务示例（取消注释即可启用）
  # - name: another-service
  #   enabled: true
  #   plugin_dir: /plugins/data/another
  #   download_url: https://example.com/releases/latest/another_linux_amd64
  #   update_interval: 3600
  #   max_retry: 10
  #   connect_timeout: "60s"

  # 禁用某个服务示例（enabled: false）
  # - name: disabled-service
  #   enabled: false
  #   plugin_dir: /plugins/data/disabled
  #   download_url: https://example.com/disabled
`

func ensureConfig(path string) error {
	_, err := os.Stat(path)
	if err == nil {
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	logGlobal(fmt.Sprintf("未找到配置文件 %s，正在生成默认模板...", path))
	if err := os.WriteFile(path, []byte(defaultConfigYAML), 0o644); err != nil {
		return err
	}
	logGlobalOK(fmt.Sprintf("已生成默认配置: %s（请根据需要修改后重新运行）", path))
	return nil
}