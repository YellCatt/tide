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
)

// sleepCtx 可被中断的 sleep，返回 false 表示已被取消
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

// ============ 清理函数 ============
func shutdown(code int) {
	cleanupOne.Do(func() {
		logInfo("收到退出信号，开始清理...")
		running.Store(false)
		if cancelApp != nil {
			cancelApp()
		}
		killProgram()
		_ = os.Remove(pidFilePath)
		_ = os.Remove(tmpPath)
		logOK("清理完成，脚本退出")
		closeLog()
	})
	os.Exit(code)
}

func cleanup() { shutdown(0) }

// fatalExit 启动阶段的致命错误：清理痕迹后以非 0 退出
func fatalExit(msg string) {
	logError(msg)
	_ = os.Remove(pidFilePath)
	_ = os.Remove(tmpPath)
	closeLog()
	os.Exit(1)
}

// ============ 主守护循环 ============
func mainLoop(ctx context.Context) {
	// 首次启动
	if err := startProgram(); err != nil {
		logWarn("本地版本不存在，尝试下载...")
		if !downloadBinary(ctx) {
			logError("下载失败且无本地版本，无法启动")
			return
		}
		if err := applyUpdate(); err != nil {
			logError(fmt.Sprintf("替换二进制失败: %v", err))
			return
		}
		if err := startProgram(); err != nil {
			logError("程序启动失败，守护循环终止")
			return
		}
	}
	currentDelay.Store(restartDelay)

	// 初始化 4 字段更新记录
	initUpdateRecord()

	firstCheck := true
	hasChild := true

	for running.Load() {
		if child.running() {
			if firstCheck {
				logStep("程序已启动，立即后台检查新版本...")
				firstCheck = false
				if downloadBinary(ctx) {
					if _, err := os.Stat(binaryPath); err == nil && fileEquals(tmpPath, binaryPath) {
						logInfo("当前已是最新版本，无需替换")
						_ = os.Remove(tmpPath)
					} else {
						logOK("发现新版本，准备热更新")
						needUpdate.Store(true)
					}
				} else {
					logWarn("启动后更新检查失败，继续使用当前版本")
				}
			} else {
				checkAndUpdate(ctx)
			}

			if needUpdate.Load() {
				logStep("执行热更新...")
				stopProgram()
				hasChild = false
				if err := applyUpdate(); err != nil {
					logError(fmt.Sprintf("替换二进制失败: %v", err))
				} else {
					logOK("已替换为新版本")
				}
				needUpdate.Store(false)
				if err := startProgram(); err != nil {
					logError("热更新后启动失败，守护循环终止")
					break
				}
				hasChild = true
				currentDelay.Store(restartDelay)
			} else if !sleepCtx(ctx, loopIdleInterval) {
				break
			}
			continue
		}

		// 子进程已退出
		if hasChild {
			exitCode := child.lastExitCode()
			logRaw("========================================")
			logInfo(fmt.Sprintf("程序已退出，退出码: %d", exitCode))
			switch {
			case exitCode == 0:
				logInfo("状态: 正常退出")
			case exitCode == 143 || exitCode == 130:
				logInfo("状态: 被信号终止（守护脚本主动停止或热更新，属正常）")
			default:
				logError("状态: 异常退出")
			}
			child.clear()
			hasChild = false
		}

		checkAndUpdate(ctx)
		if needUpdate.Load() {
			if err := applyUpdate(); err != nil {
				logError(fmt.Sprintf("替换二进制失败: %v", err))
			} else {
				logOK("已更新到新版本")
			}
			needUpdate.Store(false)
		}

		delay := currentDelay.Load()
		logInfo(fmt.Sprintf("等待 %d 秒后重启...", delay))
		if !sleepCtx(ctx, time.Duration(delay)*time.Second) {
			break
		}
		delay *= 2
		if delay > maxRestartDelay {
			delay = maxRestartDelay
		}
		currentDelay.Store(delay)

		if err := startProgram(); err != nil {
			logError("重启失败，守护循环终止")
			break
		}
		hasChild = true
		currentDelay.Store(restartDelay)
	}
}

func main() {
	// 预创建目录，确保早期日志能写入
	initLog()

	appCtx, cancelApp = context.WithCancel(context.Background())
	running.Store(true)
	currentDelay.Store(restartDelay)

	// 捕获 INT / TERM，等价于 trap 'cleanup' INT TERM
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigCh
		logInfo(fmt.Sprintf("收到退出信号 (%v)", s))
		cleanup()
	}()

	// ============ 启动信息 ============
	logRaw("========================================")
	logInfo("glean 守护脚本启动")
	if wd, err := os.Getwd(); err == nil {
		logInfo(fmt.Sprintf("当前工作目录: %s", wd))
	}
	logInfo(fmt.Sprintf("插件目录: %s", pluginDir))
	logInfo(fmt.Sprintf("下载地址: %s", downloadURL))
	logInfo(fmt.Sprintf("最大下载重试: %d 次", maxRetry))
	logInfo(fmt.Sprintf("更新检查间隔: %d 秒", updateInterval))
	logInfo(fmt.Sprintf("连接超时: %d 秒", int(connectTimeout.Seconds())))
	logInfo(fmt.Sprintf("单次下载最大耗时: %d 秒", int(maxDownloadTime.Seconds())))

	// ============ 防重复启动 ============
	if pid, ok := readPidFile(); ok {
		if isProcessAlive(pid) {
			logError(fmt.Sprintf("检测到已有实例在运行 (PID: %d)，请勿重复启动", pid))
			closeLog()
			os.Exit(1)
		}
		logWarn("发现残留 PID 文件，但对应进程已不存在，继续启动")
		_ = os.Remove(pidFilePath)
	}

	// ============ 启动前强制清理残留进程 ============
	killLeftoverProcesses()
	_ = os.Remove(pidFilePath)
	logInfo("已清理可能残留的 glean 进程和 PID 文件")

	if err := writePidFile(); err != nil {
		fatalExit(fmt.Sprintf("PID 文件写入失败: %v", err))
	}
	logOK(fmt.Sprintf("PID 文件已写入: %s (当前 PID: %d)", pidFilePath, os.Getpid()))

	// ============ 等待网络就绪 ============
	waitForNetwork(appCtx)
	logRaw("========================================")

	// ============ 检查并创建插件目录 ============
	logStep("检查插件目录...")
	if st, err := os.Stat(pluginDir); err != nil || !st.IsDir() {
		logInfo(fmt.Sprintf("目录不存在，正在创建: %s", pluginDir))
		if err := os.MkdirAll(pluginDir, 0o755); err != nil {
			fatalExit("目录创建失败，退出")
		}
		logOK("目录创建成功")
	} else {
		logOK(fmt.Sprintf("目录已存在: %s", pluginDir))
	}

	// ============ 进入插件目录 ============
	if err := os.Chdir(pluginDir); err != nil {
		fatalExit(fmt.Sprintf("进入目录失败: %s", pluginDir))
	}

	mainLoop(appCtx)
	cleanup()
}
