package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ============ .last_update_check 解析 ============
// 新格式：last_check_ts|last_human|next_check_ts_num|next_check_human
// 兼容旧 3 字段 / 旧 2 字段自动升级。
func parseUpdateFile() (lastTS int64, lastHuman string, nextTS int64, nextHuman string) {
	data, err := os.ReadFile(updateRecord)
	if err != nil {
		return 0, "", 0, ""
	}
	line := strings.TrimSpace(string(data))
	fields := strings.Split(line, "|")

	atoi := func(s string) int64 {
		v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return 0
		}
		return v
	}

	switch {
	case len(fields) >= 4: // 4 字段新格式
		lastTS = atoi(fields[0])
		lastHuman = strings.TrimSpace(fields[1])
		nextTS = atoi(fields[2])
		nextHuman = strings.TrimSpace(fields[3])
	case len(fields) == 3: // 旧 3 字段：last|last_h|next_num
		lastTS = atoi(fields[0])
		lastHuman = strings.TrimSpace(fields[1])
		nextTS = atoi(fields[2])
		nextHuman = "(未记录，旧配置)"
	case len(fields) == 2: // 旧 2 字段
		lastTS = atoi(fields[0])
		lastHuman = strings.TrimSpace(fields[1])
		nextTS = lastTS + updateInterval
		nextHuman = "(未记录，旧配置)"
	default:
		return 0, "", 0, ""
	}
	return lastTS, lastHuman, nextTS, nextHuman
}

func writeUpdateRecord(lastTS int64, lastHuman string, nextTS int64, nextHuman string) {
	content := fmt.Sprintf("%d|%s|%d|%s\n", lastTS, lastHuman, nextTS, nextHuman)
	if err := os.WriteFile(updateRecord, []byte(content), 0o644); err != nil {
		logWarn(fmt.Sprintf("写入更新记录失败: %v", err))
	}
}

func humanNow() string {
	return time.Now().Format(timeLayout)
}

func humanUnix(ts int64) string {
	return time.Unix(ts, 0).Format(timeLayout)
}

// ============ 将秒数转换为人类可读的时长 ============
func formatDuration(total int64) string {
	if total < 0 {
		total = 0
	}
	days := total / 86400
	hours := (total % 86400) / 3600
	mins := (total % 3600) / 60
	secs := total % 60

	var b strings.Builder
	if days > 0 {
		fmt.Fprintf(&b, "%d天", days)
	}
	if hours > 0 {
		fmt.Fprintf(&b, "%d小时", hours)
	}
	if mins > 0 {
		fmt.Fprintf(&b, "%d分", mins)
	}
	if secs > 0 || b.Len() == 0 {
		fmt.Fprintf(&b, "%d秒", secs)
	}
	return b.String()
}

// ============ 更新检查 ============
// checkAndUpdate 到点才检查更新，下载成功且内容变化时置 NEED_UPDATE。
func checkAndUpdate(ctx context.Context) bool {
	now := time.Now().Unix()
	lastTS, _, nextTS, _ := parseUpdateFile()

	// 调度判断只使用数字时间戳
	if now < nextTS {
		return true
	}

	elapsed := now - lastTS
	logStep(fmt.Sprintf("距离上次更新已 %s，开始下载最新版本...", formatDuration(elapsed)))

	newLastTS := now
	newLastHuman := humanNow()
	newNextTS := newLastTS + updateInterval
	newNextHuman := humanUnix(newNextTS)

	writeUpdateRecord(newLastTS, newLastHuman, newNextTS, newNextHuman)
	logInfo(fmt.Sprintf("本次更新检查完成，预计下次更新检查: %s (ts=%d)", newNextHuman, newNextTS))

	if !downloadBinary(ctx) {
		logWarn("下载失败，继续使用当前版本")
		return false
	}

	if _, err := os.Stat(binaryPath); err == nil {
		if fileEquals(tmpPath, binaryPath) {
			logInfo("下载的文件与当前版本一致，无需替换")
			_ = os.Remove(tmpPath)
			return true
		}
		logInfo("下载的文件与当前版本不同，准备替换")
	} else {
		logInfo("当前无旧版本，直接启用新版本")
	}

	needUpdate.Store(true)
	return true
}

// applyUpdate 用下载好的临时文件替换正式二进制
func applyUpdate() error {
	if err := os.Rename(tmpPath, binaryPath); err != nil {
		return err
	}
	return os.Chmod(binaryPath, 0o755)
}

// initUpdateRecord 初始化 4 字段更新记录
func initUpdateRecord() {
	now := time.Now().Unix()
	next := now + updateInterval
	writeUpdateRecord(now, humanNow(), next, humanUnix(next))
	logInfo(fmt.Sprintf("初始化更新记录，预计下次更新检查: %s (ts=%d)", humanUnix(next), next))
}
