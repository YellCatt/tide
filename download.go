package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"time"
)

func waitForNetwork(ctx context.Context) {
	logGlobalStep("等待网络就绪...")
	waited := 0
	for {
		if networkReady() {
			logGlobalOK(fmt.Sprintf("网络已就绪 (累计等待 %d 秒)", waited))
			return
		}
		waited += 5
		if waited%15 == 0 {
			logGlobalWarn(fmt.Sprintf("网络未就绪，已等待 %d 秒，继续等待...", waited))
		}
		if !sleepCtx(ctx, 5*time.Second) {
			return
		}
	}
}

func networkReady() bool {
	if path, err := exec.LookPath("ping"); err == nil {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := exec.CommandContext(cctx, path, "-c", "1", "-W", "3", "8.8.8.8").Run(); err == nil {
			return true
		}
	}
	conn, err := net.DialTimeout("tcp", "8.8.8.8:53", 3*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (s *Service) newHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   s.Cfg.ConnectTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   s.Cfg.ConnectTimeout,
		ResponseHeaderTimeout: s.Cfg.ConnectTimeout,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          10,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		DisableCompression:    true,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   s.Cfg.MaxDownloadTime,
	}
}

func (s *Service) downloadToFile(ctx context.Context, dst, url string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "tide")

	client := s.newHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP 状态码 %d", resp.StatusCode)
	}

	f, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	written, err := io.Copy(f, resp.Body)
	cerr := f.Close()
	if err != nil {
		return written, err
	}
	if cerr != nil {
		return written, cerr
	}
	if written == 0 {
		return 0, errors.New("下载内容为空")
	}
	return written, nil
}

func (s *Service) downloadBinary(ctx context.Context) bool {
	s.logStep("尝试从 GitHub 下载最新版本...")
	_ = os.Remove(s.Cfg.TmpPath)

	for retry := 1; retry <= s.Cfg.MaxRetry; retry++ {
		startWall := time.Now().Format("2006-01-02 15:04:05")
		s.logInfo(fmt.Sprintf("第 %d / %d 次下载尝试，开始时刻:%s (连接超时 %ds, 最大耗时 %ds)",
			retry, s.Cfg.MaxRetry, startWall,
			int(s.Cfg.ConnectTimeout.Seconds()), int(s.Cfg.MaxDownloadTime.Seconds())))

		size, err := s.downloadToFile(ctx, s.Cfg.TmpPath, s.Cfg.DownloadURL)

		endWall := time.Now().Format("2006-01-02 15:04:05")
		if err == nil {
			s.logInfo(fmt.Sprintf("第 %d 次下载结束时刻:%s, 结果=成功", retry, endWall))
		} else {
			s.logInfo(fmt.Sprintf("第 %d 次下载结束时刻:%s, 结果=失败", retry, endWall))
		}

		if err == nil {
			if chmodErr := os.Chmod(s.Cfg.TmpPath, 0o755); chmodErr != nil {
				s.logError(fmt.Sprintf("添加执行权限失败: %v", chmodErr))
				_ = os.Remove(s.Cfg.TmpPath)
				continue
			}
			s.logOK(fmt.Sprintf("下载成功，文件大小: %s，已添加执行权限", humanSize(size)))
			return true
		}

		s.logError(fmt.Sprintf("下载失败: %v", err))
		_ = os.Remove(s.Cfg.TmpPath)
		if retry < s.Cfg.MaxRetry {
			s.logInfo(fmt.Sprintf("等待 %d 秒后重试...", int(s.Cfg.DownloadRetryDelay.Seconds())))
			if !sleepCtx(ctx, s.Cfg.DownloadRetryDelay) {
				return false
			}
		}
	}

	s.logError(fmt.Sprintf("已达到最大重试次数 (%d)，下载失败", s.Cfg.MaxRetry))
	return false
}

func fileEquals(a, b string) bool {
	fa, err := os.Open(a)
	if err != nil {
		return false
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false
	}
	defer fb.Close()

	sa, err := fa.Stat()
	if err != nil {
		return false
	}
	sb, err := fb.Stat()
	if err != nil {
		return false
	}
	if sa.Size() != sb.Size() {
		return false
	}

	bufA := make([]byte, 64*1024)
	bufB := make([]byte, 64*1024)
	for {
		na, ea := io.ReadFull(fa, bufA)
		nb, eb := io.ReadFull(fb, bufB)
		if na != nb || !bytes.Equal(bufA[:na], bufB[:nb]) {
			return false
		}
		if ea == io.EOF || ea == io.ErrUnexpectedEOF {
			return eb == io.EOF || eb == io.ErrUnexpectedEOF
		}
		if ea != nil || eb != nil {
			return false
		}
	}
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(n)/float64(div), "KMGTPE"[exp])
}