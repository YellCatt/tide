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

// ============ 网络就绪检测 ============
// waitForNetwork 等价于 `while ! ping -c 1 -W 3 8.8.8.8; do sleep 5; done`
func waitForNetwork(ctx context.Context) {
	logStep("等待网络就绪...")
	waited := 0
	for {
		if networkReady() {
			logOK(fmt.Sprintf("网络已就绪 (累计等待 %d 秒)", waited))
			return
		}
		waited += 5
		if waited%15 == 0 {
			logWarn(fmt.Sprintf("网络未就绪，已等待 %d 秒，继续等待...", waited))
		}
		if !sleepCtx(ctx, networkCheckInterval) {
			return
		}
	}
}

// networkReady 先尝试 ping，失败再退化到 TCP 探测（部分环境 ICMP 被禁但网络可用）。
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

// ============ 下载 ============
func newHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   connectTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   connectTimeout,
		ResponseHeaderTimeout: connectTimeout,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          10,
		// 等价于 curl -k：跳过证书校验
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		// 二进制下载，禁止透明压缩
		DisableCompression: true,
	}
	return &http.Client{
		Transport: transport,
		// 等价于 curl --max-time：单次下载整体最大耗时
		Timeout: maxDownloadTime,
	}
}

var httpClient = newHTTPClient()

// downloadToFile 等价于 curl -L -k --connect-timeout N --max-time M -s -o TMP URL
func downloadToFile(ctx context.Context, dst, url string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "glean-guard")

	resp, err := httpClient.Do(req)
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

// downloadBinary 下载到临时文件并在成功后 chmod +x，失败自动重试。
func downloadBinary(ctx context.Context) bool {
	logStep("尝试从 GitHub 下载最新版本...")
	_ = os.Remove(tmpPath)

	for retry := 1; retry <= maxRetry; retry++ {
		startWall := time.Now().Format("2006-01-02 15:04:05")
		logInfo(fmt.Sprintf("第 %d / %d 次下载尝试，开始时刻:%s (连接超时 %ds, 最大耗时 %ds)",
			retry, maxRetry, startWall,
			int(connectTimeout.Seconds()), int(maxDownloadTime.Seconds())))

		size, err := downloadToFile(ctx, tmpPath, downloadURL)

		endWall := time.Now().Format("2006-01-02 15:04:05")
		if err == nil {
			logInfo(fmt.Sprintf("第 %d 次下载结束时刻:%s, 结果=成功", retry, endWall))
		} else {
			logInfo(fmt.Sprintf("第 %d 次下载结束时刻:%s, 结果=失败", retry, endWall))
		}

		if err == nil {
			if chmodErr := os.Chmod(tmpPath, 0o755); chmodErr != nil {
				logError(fmt.Sprintf("添加执行权限失败: %v", chmodErr))
				_ = os.Remove(tmpPath)
				continue
			}
			logOK(fmt.Sprintf("下载成功，文件大小: %s，已添加执行权限", humanSize(size)))
			return true
		}

		logError(fmt.Sprintf("下载失败: %v", err))
		_ = os.Remove(tmpPath)
		if retry < maxRetry {
			logInfo(fmt.Sprintf("等待 %d 秒后重试...", int(downloadRetryDelay.Seconds())))
			if !sleepCtx(ctx, downloadRetryDelay) {
				return false
			}
		}
	}

	logError(fmt.Sprintf("已达到最大重试次数 (%d)，下载失败", maxRetry))
	return false
}

// ============ 工具函数 ============
// fileEquals 等价于 `cmp -s a b`
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

// humanSize 等价于 `ls -lh` 的大小列
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
