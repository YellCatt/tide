package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func humanNow() string {
	return time.Now().Format(timeLayout)
}

func humanUnix(ts int64) string {
	return time.Unix(ts, 0).Format(timeLayout)
}

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

func (s *Service) parseUpdateFile() (lastTS int64, lastHuman string, nextTS int64, nextHuman string) {
	data, err := os.ReadFile(s.Cfg.UpdateRecord)
	if err != nil {
		return 0, "", 0, ""
	}
	line := strings.TrimSpace(string(data))
	fields := strings.Split(line, "|")

	atoi := func(st string) int64 {
		v, err := strconv.ParseInt(strings.TrimSpace(st), 10, 64)
		if err != nil {
			return 0
		}
		return v
	}

	switch {
	case len(fields) >= 4:
		lastTS = atoi(fields[0])
		lastHuman = strings.TrimSpace(fields[1])
		nextTS = atoi(fields[2])
		nextHuman = strings.TrimSpace(fields[3])
	case len(fields) == 3:
		lastTS = atoi(fields[0])
		lastHuman = strings.TrimSpace(fields[1])
		nextTS = atoi(fields[2])
		nextHuman = "(未记录，旧配置)"
	case len(fields) == 2:
		lastTS = atoi(fields[0])
		lastHuman = strings.TrimSpace(fields[1])
		nextTS = lastTS + int64(s.Cfg.UpdateInterval)
		nextHuman = "(未记录，旧配置)"
	default:
		return 0, "", 0, ""
	}
	return lastTS, lastHuman, nextTS, nextHuman
}

func (s *Service) writeUpdateRecord(lastTS int64, lastHuman string, nextTS int64, nextHuman string) {
	content := fmt.Sprintf("%d|%s|%d|%s\n", lastTS, lastHuman, nextTS, nextHuman)
	if err := os.WriteFile(s.Cfg.UpdateRecord, []byte(content), 0o644); err != nil {
		s.logWarn(fmt.Sprintf("写入更新记录失败: %v", err))
	}
}

func (s *Service) checkAndUpdate(ctx context.Context) bool {
	now := time.Now().Unix()
	lastTS, _, nextTS, _ := s.parseUpdateFile()

	if now < nextTS {
		return true
	}

	elapsed := now - lastTS
	s.logStep(fmt.Sprintf("距离上次更新已 %s，开始下载最新版本...", formatDuration(elapsed)))

	newLastTS := now
	newLastHuman := humanNow()
	newNextTS := newLastTS + int64(s.Cfg.UpdateInterval)
	newNextHuman := humanUnix(newNextTS)

	s.writeUpdateRecord(newLastTS, newLastHuman, newNextTS, newNextHuman)
	s.logInfo(fmt.Sprintf("本次更新检查完成，预计下次更新检查: %s (ts=%d)", newNextHuman, newNextTS))

	if !s.downloadBinary(ctx) {
		s.logWarn("下载失败，继续使用当前版本")
		return false
	}

	if _, err := os.Stat(s.Cfg.BinaryPath); err == nil {
		if fileEquals(s.Cfg.TmpPath, s.Cfg.BinaryPath) {
			s.logInfo("下载的文件与当前版本一致，无需替换")
			_ = os.Remove(s.Cfg.TmpPath)
			return true
		}
		s.logInfo("下载的文件与当前版本不同，准备替换")
	} else {
		s.logInfo("当前无旧版本，直接启用新版本")
	}

	s.needUpdate.Store(true)
	return true
}

func (s *Service) applyUpdate() error {
	if err := os.Rename(s.Cfg.TmpPath, s.Cfg.BinaryPath); err != nil {
		return err
	}
	return os.Chmod(s.Cfg.BinaryPath, 0o755)
}

func (s *Service) initUpdateRecord() {
	now := time.Now().Unix()
	next := now + int64(s.Cfg.UpdateInterval)
	s.writeUpdateRecord(now, humanNow(), next, humanUnix(next))
	s.logInfo(fmt.Sprintf("初始化更新记录，预计下次更新检查: %s (ts=%d)", humanUnix(next), next))
}