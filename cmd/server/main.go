package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/scheduler"
	"codearts2api/internal/server"
	"codearts2api/internal/telemetry"
	"codearts2api/internal/upstream"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	settingsPath := filepath.Join(filepath.Dir(*cfgPath), "data", "settings.json")
	if cfg.StateFile != "" {
		settingsPath = filepath.Join(filepath.Dir(cfg.StateFile), "settings.json")
	}
	settings, err := server.OpenSettings(settingsPath, settingsFromConfig(*cfg))
	if err != nil {
		log.Fatalf("load settings: %v", err)
	}
	applySettingsToConfig(cfg, settings.Get())

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)
	if len(auths) == 0 {
		log.Printf("WARNING: no accounts found. Run `codearts2api login` first.")
	}

	if cfg.StateFile != "" {
		_ = os.MkdirAll(filepath.Dir(cfg.StateFile), 0o700)
	}
	var telemetryStore *telemetry.Store
	if cfg.Observability.Enabled {
		telemetryStore, err = telemetry.Open(cfg.Observability.SQLitePath)
		if err != nil {
			log.Fatalf("open telemetry store: %v", err)
		}
		if _, err := telemetryStore.Purge(
			cfg.Observability.RetentionDays,
			cfg.Observability.MaxRows,
			cfg.Observability.MaxDBBytes,
		); err != nil {
			log.Printf("telemetry purge failed: %v", err)
		}
	}
	poolConfig := cfg.ToPoolConfig()
	poolConfig.Telemetry = telemetryStore
	p, err := pool.New(auths, poolConfig, cfg.StateFile)
	if err != nil {
		log.Fatalf("build pool: %v", err)
	}

	c := upstream.New(poolConfig.UpstreamTimeout)
	c.SetBenefitAutoClaim(cfg.BenefitAutoClaim)

	sch := scheduler.New(scheduler.Config{
		Pool:              p,
		Enabled:           cfg.Watch.Enabled,
		CreditsAutoClaim:  cfg.CreditsAutoClaim,
		PollInterval:      time.Duration(cfg.Watch.PollMinutes) * time.Minute,
		RefreshSkew:       time.Duration(cfg.Watch.RefreshSkewM) * time.Minute,
		KeepaliveInterval: time.Duration(cfg.Watch.KeepaliveInterval) * time.Minute,
	})

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     c,
		APIKey:       cfg.APIKey,
		MaxRotate:    3,
		SoftCooldown: cfg.SoftRateDur,
		ErrThreshold: cfg.Cooldown.ErrThresh,
		ErrCooldown:  cfg.ErrCooldownDur,
		DefaultModel: cfg.DefaultModel,
		QueueRetryDelay: func() time.Duration {
			if cfg.QueueRetrySeconds > 0 {
				return time.Duration(cfg.QueueRetrySeconds) * time.Second
			}
			return 10 * time.Second
		}(),
		QueueMaxAttempts: cfg.QueueMaxAttempts,
		ConvStateFile:    cfg.StateFile + ".chats.json",
		WatchInfo: map[string]any{
			"enabled":              cfg.Watch.Enabled,
			"credits_auto_claim":   cfg.CreditsAutoClaim,
			"poll_minutes":         cfg.Watch.PollMinutes,
			"refresh_skew_minutes": cfg.Watch.RefreshSkewM,
			"keepalive_interval":   cfg.Watch.KeepaliveInterval,
			"max_concurrent":       cfg.MaxConcurrent,
			"keepalive_window":     poolConfig.KeepaliveWindow.String(),
		},
		AuthDir:                    cfg.AuthDir,
		Listen:                     cfg.Listen,
		OAuthClient:                upstream.New(15 * time.Second),
		Telemetry:                  telemetryStore,
		ObservabilityRetentionDays: cfg.Observability.RetentionDays,
		ObservabilityMaxRows:       cfg.Observability.MaxRows,
		ObservabilityMaxDBBytes:    cfg.Observability.MaxDBBytes,
		SnapshotInterval:           time.Duration(cfg.Observability.SnapshotIntervalSeconds) * time.Second,
		LoginConfig:                loginConfig(cfg),
		OAuthCallbackHost:          cfg.OAuthCallbackHost,
		Settings:                   settings,
		Scheduler:                  sch,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)
	h.StartAvailabilityProber(ctx)
	h.StartSnapshotter(ctx)
	if telemetryStore != nil {
		go func() {
			ticker := time.NewTicker(time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if _, err := telemetryStore.Purge(
						cfg.Observability.RetentionDays,
						cfg.Observability.MaxRows,
						cfg.Observability.MaxDBBytes,
					); err != nil {
						log.Printf("telemetry purge failed: %v", err)
					}
				}
			}
		}()
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
	}
	if telemetryStore != nil {
		defer telemetryStore.Close()
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("codearts2api listening on %s (api_key=%v)", cfg.Listen, cfg.APIKey != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	h.FlushConversationState()
	log.Printf("bye")
}

// loginConfig 决定 WebUI 登录使用的 OAuth 配置。
//
// client_id 优先取已有账号记录的取值（refresh_token 与 client_id 绑定，混用会让
// 老账号刷新失败），其次取配置项，最后才是内置默认值。其余字段必须来自
// DefaultLoginConfig（否则 authorize 链接会缺 host / plugin 信息）。
func loginConfig(cfg *Config) upstream.LoginConfig {
	lc := upstream.DefaultLoginConfig()
	if cfg.LoginClientID != "" {
		lc.ClientID = cfg.LoginClientID
	}
	if auths, err := auth.LoadDir(cfg.AuthDir); err == nil {
		for _, a := range auths {
			if id := a.ClientIDOr(""); id != "" {
				lc.ClientID = id
				break
			}
		}
	}
	return lc
}

func settingsFromConfig(cfg Config) server.Settings {
	// config.json 缺省的字段回退 DefaultSettings，避免零值进入设置
	// （历史问题：queue 字段缺省时设置页显示 0，保存会被范围校验拒绝）。
	settings := server.DefaultSettings()
	if cfg.DefaultModel != "" {
		settings.DefaultModel = cfg.DefaultModel
	}
	if cfg.MaxConcurrent > 0 {
		settings.MaxConcurrent = cfg.MaxConcurrent
	}
	if cfg.QueueRetrySeconds > 0 {
		settings.QueueRetrySeconds = cfg.QueueRetrySeconds
	}
	if cfg.QueueMaxAttempts > 0 {
		settings.QueueMaxAttempts = cfg.QueueMaxAttempts
	}
	settings.BenefitAutoClaim = cfg.BenefitAutoClaim
	settings.CreditsAutoClaim = cfg.CreditsAutoClaim
	settings.WatchEnabled = cfg.Watch.Enabled
	if cfg.Watch.PollMinutes > 0 {
		settings.PollMinutes = cfg.Watch.PollMinutes
	}
	if cfg.Watch.RefreshSkewM > 0 {
		settings.RefreshSkewMinutes = cfg.Watch.RefreshSkewM
	}
	if cfg.Watch.KeepaliveInterval > 0 {
		settings.KeepaliveIntervalMinutes = cfg.Watch.KeepaliveInterval
	}
	settings.KeepaliveWindowMinutes = int(poolConfigKeepaliveWindow(cfg).Minutes())
	return settings
}

func applySettingsToConfig(cfg *Config, settings server.Settings) {
	cfg.DefaultModel = settings.DefaultModel
	cfg.MaxConcurrent = settings.MaxConcurrent
	cfg.QueueRetrySeconds = settings.QueueRetrySeconds
	cfg.QueueMaxAttempts = settings.QueueMaxAttempts
	cfg.BenefitAutoClaim = settings.BenefitAutoClaim
	cfg.CreditsAutoClaim = settings.CreditsAutoClaim
	cfg.Watch.Enabled = settings.WatchEnabled
	cfg.Watch.PollMinutes = settings.PollMinutes
	cfg.Watch.RefreshSkewM = settings.RefreshSkewMinutes
	cfg.Watch.KeepaliveInterval = settings.KeepaliveIntervalMinutes
	cfg.KeepaliveWindow = fmt.Sprintf("%dm", settings.KeepaliveWindowMinutes)
}

func poolConfigKeepaliveWindow(cfg Config) time.Duration {
	duration, err := time.ParseDuration(cfg.KeepaliveWindow)
	if err != nil || duration <= 0 {
		return 10 * time.Minute
	}
	return duration
}
