package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration 支持 YAML 写法 "30s" / "5m" / "1h"，也兼容纯数字（秒）
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var raw string
	if err := value.Decode(&raw); err != nil {
		var sec int64
		if err2 := value.Decode(&sec); err2 == nil {
			d.Duration = time.Duration(sec) * time.Second
			return nil
		}
		return err
	}
	if strings.TrimSpace(raw) == "" {
		d.Duration = 0
		return nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		var sec int64
		if err2 := yaml.Unmarshal([]byte(raw), &sec); err2 == nil {
			d.Duration = time.Duration(sec) * time.Second
			return nil
		}
		return fmt.Errorf("无法解析时长 %q: %w", raw, err)
	}
	d.Duration = v
	return nil
}

// GlobalConfig 对应 YAML 根节点
type GlobalConfig struct {
	Defaults ServiceBase  `yaml:"defaults"`
	Services []ServiceYAML `yaml:"services"`
}

// ServiceBase 一组可覆盖的配置字段（指针区分"未设置"和"显式设为零值"）
type ServiceBase struct {
	Enabled            *bool    `yaml:"enabled"`
	PluginDir          string   `yaml:"plugin_dir"`
	BinaryName         string   `yaml:"binary_name"`
	TmpName            string   `yaml:"tmp_name"`
	DownloadURL        string   `yaml:"download_url"`
	MaxRetry           *int     `yaml:"max_retry"`
	RestartDelay       *int     `yaml:"restart_delay"`
	MaxRestartDelay    *int     `yaml:"max_restart_delay"`
	UpdateInterval     *int     `yaml:"update_interval"`
	GracefulShutdown   *int     `yaml:"graceful_shutdown_timeout"`
	ConnectTimeout     *Duration `yaml:"connect_timeout"`
	MaxDownloadTime    *Duration `yaml:"max_download_time"`
	NetworkCheckInterval *Duration `yaml:"network_check_interval"`
	DownloadRetryDelay   *Duration `yaml:"download_retry_delay"`
	LoopIdleInterval    *Duration `yaml:"loop_idle_interval"`
}

// ServiceYAML YAML 里 services 数组的一项，name 必填
type ServiceYAML struct {
	Name string `yaml:"name"`
	ServiceBase `yaml:",inline"`
}

// 最终展开后的服务配置快照（所有字段都是具体值，无指针）
type ServiceConfig struct {
	Name    string
	Enabled bool

	PluginDir       string
	BinaryName      string
	TmpName         string
	DownloadURL     string
	MaxRetry        int
	RestartDelay    int
	MaxRestartDelay int
	UpdateInterval  int

	GracefulShutdownTimeout time.Duration
	ConnectTimeout          time.Duration
	MaxDownloadTime         time.Duration
	NetworkCheckInterval    time.Duration
	DownloadRetryDelay      time.Duration
	LoopIdleInterval        time.Duration

	// 派生路径
	LogFilePath  string
	PidFilePath  string
	BinaryPath   string
	TmpPath      string
	UpdateRecord string
}

func defaultBase() ServiceBase {
	rd := 5
	mrd := 300
	ui := 14400
	gs := 10
	ct := Duration{120 * time.Second}
	mdt := Duration{1200 * time.Second}
	nci := Duration{5 * time.Second}
	drd := Duration{10 * time.Second}
	liid := Duration{10 * time.Second}
	mr := 20
	en := true
	return ServiceBase{
		Enabled:            &en,
		PluginDir:          "",
		BinaryName:         "",
		TmpName:            "",
		DownloadURL:        "",
		MaxRetry:           &mr,
		RestartDelay:       &rd,
		MaxRestartDelay:    &mrd,
		UpdateInterval:     &ui,
		GracefulShutdown:   &gs,
		ConnectTimeout:     &ct,
		MaxDownloadTime:    &mdt,
		NetworkCheckInterval: &nci,
		DownloadRetryDelay:   &drd,
		LoopIdleInterval:    &liid,
	}
}

func mergeBase(dst, src ServiceBase) ServiceBase {
	if src.Enabled != nil {
		dst.Enabled = src.Enabled
	}
	if src.PluginDir != "" {
		dst.PluginDir = src.PluginDir
	}
	if src.BinaryName != "" {
		dst.BinaryName = src.BinaryName
	}
	if src.TmpName != "" {
		dst.TmpName = src.TmpName
	}
	if src.DownloadURL != "" {
		dst.DownloadURL = src.DownloadURL
	}
	if src.MaxRetry != nil {
		dst.MaxRetry = src.MaxRetry
	}
	if src.RestartDelay != nil {
		dst.RestartDelay = src.RestartDelay
	}
	if src.MaxRestartDelay != nil {
		dst.MaxRestartDelay = src.MaxRestartDelay
	}
	if src.UpdateInterval != nil {
		dst.UpdateInterval = src.UpdateInterval
	}
	if src.GracefulShutdown != nil {
		dst.GracefulShutdown = src.GracefulShutdown
	}
	if src.ConnectTimeout != nil {
		dst.ConnectTimeout = src.ConnectTimeout
	}
	if src.MaxDownloadTime != nil {
		dst.MaxDownloadTime = src.MaxDownloadTime
	}
	if src.NetworkCheckInterval != nil {
		dst.NetworkCheckInterval = src.NetworkCheckInterval
	}
	if src.DownloadRetryDelay != nil {
		dst.DownloadRetryDelay = src.DownloadRetryDelay
	}
	if src.LoopIdleInterval != nil {
		dst.LoopIdleInterval = src.LoopIdleInterval
	}
	return dst
}

func (y ServiceYAML) ToConfig(defaults ServiceBase) (ServiceConfig, error) {
	base := mergeBase(defaults, y.ServiceBase)

	if y.Name == "" {
		return ServiceConfig{}, fmt.Errorf("service.name 不能为空")
	}
	if base.PluginDir == "" {
		return ServiceConfig{}, fmt.Errorf("service %q: plugin_dir 未设置", y.Name)
	}

	binaryName := base.BinaryName
	if binaryName == "" {
		binaryName = y.Name
	}
	tmpName := base.TmpName
	if tmpName == "" {
		tmpName = binaryName + ".tmp"
	}

	cfg := ServiceConfig{
		Name:                    y.Name,
		Enabled:                 *base.Enabled,
		PluginDir:               base.PluginDir,
		BinaryName:              binaryName,
		TmpName:                 tmpName,
		DownloadURL:             base.DownloadURL,
		MaxRetry:                *base.MaxRetry,
		RestartDelay:            *base.RestartDelay,
		MaxRestartDelay:         *base.MaxRestartDelay,
		UpdateInterval:          *base.UpdateInterval,
		GracefulShutdownTimeout: baseDuration(base.GracefulShutdown, 0),
		ConnectTimeout:          base.ConnectTimeout.Duration,
		MaxDownloadTime:         base.MaxDownloadTime.Duration,
		NetworkCheckInterval:    base.NetworkCheckInterval.Duration,
		DownloadRetryDelay:      base.DownloadRetryDelay.Duration,
		LoopIdleInterval:        base.LoopIdleInterval.Duration,
		LogFilePath:             filepath.Join(base.PluginDir, "logs", binaryName+".log"),
		PidFilePath:             filepath.Join(base.PluginDir, binaryName+".pid"),
		BinaryPath:              filepath.Join(base.PluginDir, binaryName),
		TmpPath:                 filepath.Join(base.PluginDir, tmpName),
		UpdateRecord:            filepath.Join(base.PluginDir, ".last_update_check"),
	}
	return cfg, nil
}

func baseDuration(secPtr *int, fallback time.Duration) time.Duration {
	if secPtr != nil {
		return time.Duration(*secPtr) * time.Second
	}
	return fallback
}

func LoadConfig(path string) (GlobalConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return GlobalConfig{}, err
	}
	var raw GlobalConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return GlobalConfig{}, fmt.Errorf("解析 YAML 失败: %w", err)
	}

	defaults := mergeBase(defaultBase(), raw.Defaults)

	return GlobalConfig{
		Defaults: defaults,
		Services: raw.Services,
	}, nil
}