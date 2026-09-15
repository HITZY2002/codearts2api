package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"codearts2api/internal/pool"
)

// Config 顶层配置。
type Config struct {
	Listen       string `json:"listen"`
	APIKey       string `json:"api_key"` // config.json 的 api_key 或 env CA2A_API_KEY（同时存在时 env 优先）
	AuthDir      string `json:"auth_dir"`
	StateFile    string `json:"state_file"`
	DefaultModel string `json:"default_model"`
	// OAuthCallbackHost 可选：远程授权时把回调指向公网地址，让浏览器回调经反向
	// 代理进入本服务，而不是 127.0.0.1。只有**显式写了端口**才会改写授权链接里的
	// port 参数（如 https://oneapi.example.com:443/codearts）；不写端口则保持本机
	// 端口不变。远端回调不通时会自动回退到 ticket 轮询通道，不影响登录完成。
	OAuthCallbackHost string `json:"oauth_callback_host,omitempty"`

	Cooldown struct {
		SoftRate    string `json:"soft_rate"`
		ErrThresh   int    `json:"err_threshold"`
		ErrCooldown string `json:"err_cooldown"`
	} `json:"cooldown"`

	Watch struct {
		Enabled           bool `json:"enabled"`
		PollMinutes       int  `json:"poll_minutes"`
		RefreshSkewM      int  `json:"refresh_skew_minutes"`
		KeepaliveInterval int  `json:"keepalive_interval_minutes"` // 保活心跳间隔（新增）
	} `json:"watch"`

	MaxConcurrent   int    `json:"max_concurrent"`   // 单账号最大并发数（新增）
	KeepaliveWindow string `json:"keepalive_window"` // 保活窗口（新增）

	// LoginClientID WebUI 登录使用的 OAuth client_id（留空用默认 codearts-agent）。
	LoginClientID string `json:"login_client_id"`

	// QueueRetrySeconds 上游并发/TPM 排队时的重试间隔（秒，默认 10）。
	QueueRetrySeconds int `json:"queue_retry_seconds"`
	// QueueMaxAttempts 排队重试次数上限（默认 30，约 5 分钟）。
	QueueMaxAttempts int `json:"queue_max_attempts"`

	// BenefitAutoClaim 模型发现时自动领取限时福利（默认 true）。
	// 领取是幂等操作（官方客户端打开模型菜单即调用），不领取时福利模型一律
	// 返回 InferHub.4004.200 benefit not found。置 false 可关掉这个写操作。
	BenefitAutoClaim bool `json:"benefit_auto_claim"`

	Upstream struct {
		TimeoutSeconds int `json:"timeout_seconds"`
	} `json:"upstream"`

	SoftRateDur    time.Duration
	ErrCooldownDur time.Duration
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:       ":7866",
		AuthDir:      "./auths",
		StateFile:    "./data/state.json",
		DefaultModel: "glm-5.2",
	}
	c.Cooldown.SoftRate = "60s"
	c.Cooldown.ErrThresh = 3
	c.Cooldown.ErrCooldown = "10m"
	c.Watch.Enabled = true
	c.Watch.PollMinutes = 30
	c.Watch.RefreshSkewM = 30
	c.Watch.KeepaliveInterval = 15 // 新增：保活间隔 15 分钟
	c.MaxConcurrent = 1            // 上游单账号最多 3 并发会话但释放慢，串行最稳
	c.KeepaliveWindow = "10m"      // 新增：保活窗口 10 分钟
	c.Upstream.TimeoutSeconds = 120
	c.BenefitAutoClaim = true // 不领取时福利模型调用必然失败（benefit not found）
	return c
}

// Load 加载配置 + CA2A_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("read config: %w", err)
			}
		} else if err := json.Unmarshal(stripJSONComments(raw), c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(c)
	// 没有 API Key 就拒绝启动：旧版本回退到公开的 dummy-key-for-codearts，
	// 等于把 /v1/chat/completions 与 /v1/models 暴露给任何人（本机也可能被反代出去）。
	if c.APIKey == "" {
		return nil, fmt.Errorf("api_key 未设置：请在 config.json 写 api_key，" +
			"或设置环境变量 CA2A_API_KEY 后再启动（生成示例：openssl rand -hex 24）")
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// stripJSONComments 去掉配置里的 // 与 /* */ 注释。
//
// config.example.json 带注释（字段说明只在那一份里），README 又让人直接
// `cp config.example.json config.json`；标准 encoding/json 不接受注释，
// 因此这里先做一次剥离。字符串内的 // 与 /* 本身不动（oauth_callback_host
// 这类值就是 URL）。
func stripJSONComments(raw []byte) []byte {
	out := make([]byte, 0, len(raw))
	inString := false
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		if inString {
			out = append(out, ch)
			switch ch {
			case '\\':
				if i+1 < len(raw) {
					i++
					out = append(out, raw[i])
				}
			case '"':
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			out = append(out, ch)
			continue
		}
		if ch == '/' && i+1 < len(raw) {
			if raw[i+1] == '/' {
				for i < len(raw) && raw[i] != '\n' {
					i++
				}
				// 保留换行，尽量不改变 offset。
				if i < len(raw) {
					out = append(out, '\n')
				}
				continue
			}
			if raw[i+1] == '*' {
				i += 2
				for i+1 < len(raw) && !(raw[i] == '*' && raw[i+1] == '/') {
					i++
				}
				i++ // 跳过结尾的 '/'
				continue
			}
		}
		out = append(out, ch)
	}
	return out
}

func applyEnv(c *Config) {
	if v := os.Getenv("CA2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("CA2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("CA2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("CA2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("CA2A_DEFAULT_MODEL"); v != "" {
		c.DefaultModel = v
	}
	if v := os.Getenv("CA2A_OAUTH_CALLBACK_HOST"); v != "" {
		c.OAuthCallbackHost = v
	}
	if v := os.Getenv("CA2A_WATCH_ENABLED"); v != "" {
		c.Watch.Enabled = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("CA2A_WATCH_POLL_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Watch.PollMinutes = n
		}
	}
	if v := os.Getenv("CA2A_WATCH_REFRESH_SKEW"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Watch.RefreshSkewM = n
		}
	}
	if v := os.Getenv("CA2A_WATCH_KEEPALIVE_INTERVAL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Watch.KeepaliveInterval = n
		}
	}
	if v := os.Getenv("CA2A_MAX_CONCURRENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxConcurrent = n
		}
	}
	if v := os.Getenv("CA2A_KEEPALIVE_WINDOW"); v != "" {
		c.KeepaliveWindow = v
	}
	if v := os.Getenv("CA2A_LOGIN_CLIENT_ID"); v != "" {
		c.LoginClientID = v
	}
	if v := os.Getenv("CA2A_QUEUE_RETRY_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.QueueRetrySeconds = n
		}
	}
	if v := os.Getenv("CA2A_QUEUE_MAX_ATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.QueueMaxAttempts = n
		}
	}
	if v := os.Getenv("CA2A_BENEFIT_AUTO_CLAIM"); v != "" {
		c.BenefitAutoClaim = v == "1" || strings.EqualFold(v, "true")
	}
}

func (c *Config) normalize() error {
	var err error
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	if c.ErrCooldownDur, err = time.ParseDuration(c.Cooldown.ErrCooldown); err != nil {
		return fmt.Errorf("cooldown.err_cooldown: %w", err)
	}
	if c.Cooldown.ErrThresh <= 0 {
		c.Cooldown.ErrThresh = 3
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	if c.DefaultModel == "" {
		c.DefaultModel = "glm-5.2"
	}
	if c.Listen == "" {
		c.Listen = ":7866"
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	if c.Watch.PollMinutes <= 0 {
		c.Watch.PollMinutes = 30
	}
	if c.Watch.RefreshSkewM <= 0 {
		c.Watch.RefreshSkewM = 30
	}
	if c.Watch.KeepaliveInterval <= 0 {
		c.Watch.KeepaliveInterval = 15 // 默认保活间隔 15 分钟
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 1 // 上游并发会话释放慢，单账号串行最稳
	}
	return nil
}

// ToPoolConfig 转换为 pool.Config。
func (c *Config) ToPoolConfig() pool.Config {
	kw := c.KeepaliveWindow
	if kw == "" {
		kw = "10m"
	}
	dur, err := time.ParseDuration(kw)
	if err != nil {
		dur = 10 * time.Minute
	}
	return pool.Config{
		ErrThreshold:    c.Cooldown.ErrThresh,
		ErrCooldown:     c.ErrCooldownDur,
		SoftCooldown:    c.SoftRateDur,
		RefreshSkew:     time.Duration(c.Watch.RefreshSkewM) * time.Minute,
		MaxConcurrent:   c.MaxConcurrent,
		KeepaliveWindow: dur,
	}
}
