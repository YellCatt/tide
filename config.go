package main

import (
	"path/filepath"
	"sync/atomic"
	"time"
)

// ============ 配置区 ============
const (
	// 插件目录
	pluginDir = "/plugins/data/glean"
	// 二进制文件名
	binaryName = "glean"
	// 下载临时文件名
	tmpName = "glean.tmp"
	// 下载地址
	downloadURL = "https://github.com/YellCatt/glean/releases/download/dev-latest/default.glean_linux_mipsle"

	// 最大下载重试次数
	maxRetry = 20
	// 重启初始延迟（秒）
	restartDelay = 5
	// 重启最大延迟（秒）
	maxRestartDelay = 300
	// 更新检查间隔（秒），14400 秒 = 4 小时
	updateInterval = 14400
	// 优雅退出等待时间（秒）
	gracefulShutdownTimeout = 10
	// 单次下载连接超时
	connectTimeout = 120 * time.Second
	// 单次下载最大耗时
	maxDownloadTime = 1200 * time.Second
	// 网络就绪轮询间隔
	networkCheckInterval = 5 * time.Second
	// 下载重试间隔
	downloadRetryDelay = 10 * time.Second
	// 主循环空闲轮询间隔
	loopIdleInterval = 10 * time.Second
	// 日志与记录中的时间格式（相当于 shell 的 '%Y-%m-%d %H:%M:%S %Z (UTC%z)'）
	timeLayout = "2006-01-02 15:04:05 MST (UTC-0700)"
)

// 文件路径（由 PLUGIN_DIR 推导）
var (
	logFilePath  = filepath.Join(pluginDir, "logs", "glean.log")
	pidFilePath  = filepath.Join(pluginDir, "glean.pid")
	binaryPath   = filepath.Join(pluginDir, binaryName)
	tmpPath      = filepath.Join(pluginDir, tmpName)
	updateRecord = filepath.Join(pluginDir, ".last_update_check")
)

// ============ 全局状态 ============
var (
	// 守护进程是否继续运行
	running atomic.Bool
	// 是否需要热更新
	needUpdate atomic.Bool
	// 当前重启延迟（秒）
	currentDelay atomic.Int64
)
