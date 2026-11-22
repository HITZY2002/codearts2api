package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"codearts2api/internal/scheduler"
)

// Settings is the editable runtime subset exposed by the admin panel.
type Settings struct {
	DefaultModel             string `json:"default_model"`
	MaxConcurrent            int    `json:"max_concurrent"`
	QueueRetrySeconds        int    `json:"queue_retry_seconds"`
	QueueMaxAttempts         int    `json:"queue_max_attempts"`
	BenefitAutoClaim         bool   `json:"benefit_auto_claim"`
	CreditsAutoClaim         bool   `json:"credits_auto_claim"`
	WatchEnabled             bool   `json:"watch_enabled"`
	PollMinutes              int    `json:"poll_minutes"`
	RefreshSkewMinutes       int    `json:"refresh_skew_minutes"`
	KeepaliveIntervalMinutes int    `json:"keepalive_interval_minutes"`
	KeepaliveWindowMinutes   int    `json:"keepalive_window_minutes"`
	// /v1/models 返回内容控制：福利模型是否列出，以及要隐藏的模型 ID。
	ModelsReturnBenefit bool   `json:"models_return_benefit"`
	ModelsHidden        string `json:"models_hidden"`
}

type settingsSnapshot struct {
	Settings
	UpdatedAt string `json:"updated_at,omitempty"`
}

// SettingsStore keeps editable settings outside config.json and api keys.
type SettingsStore struct {
	mu      sync.Mutex
	path    string
	current Settings
	updated time.Time
}

func DefaultSettings() Settings {
	return Settings{
		DefaultModel:             "glm-5.2",
		MaxConcurrent:            1,
		QueueRetrySeconds:        10,
		QueueMaxAttempts:         30,
		BenefitAutoClaim:         true,
		CreditsAutoClaim:         true,
		WatchEnabled:             true,
		PollMinutes:              30,
		RefreshSkewMinutes:       30,
		KeepaliveIntervalMinutes: 15,
		KeepaliveWindowMinutes:   10,
		ModelsReturnBenefit:      true,
		ModelsHidden:             "",
	}
}

// OpenSettings loads a persisted settings file. A missing file uses fallback.
func OpenSettings(path string, fallback Settings) (*SettingsStore, error) {
	store := newSettingsStore(path, fallback)
	if path == "" {
		return store, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return store, nil
		}
		return nil, fmt.Errorf("read settings: %w", err)
	}
	var loaded Settings
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return nil, fmt.Errorf("parse settings: %w", err)
	}
	// 旧版本 settings.json 没有 models_* 字段：缺失时回退默认值，
	// 避免升级后布尔零值（false）静默关闭福利模型的返回。
	var present map[string]any
	if err := json.Unmarshal(raw, &present); err == nil {
		defaults := DefaultSettings()
		if _, ok := present["models_return_benefit"]; !ok {
			loaded.ModelsReturnBenefit = defaults.ModelsReturnBenefit
		}
		if _, ok := present["models_hidden"]; !ok {
			loaded.ModelsHidden = defaults.ModelsHidden
		}
	}
	if err := validateSettings(loaded); err != nil {
		return nil, err
	}
	store.current = loaded
	return store, nil
}

func newSettingsStore(path string, fallback Settings) *SettingsStore {
	return &SettingsStore{path: path, current: fallback}
}

func (s *SettingsStore) Get() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

func (s *SettingsStore) Update(next Settings) error {
	if err := validateSettings(next); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path != "" {
		if dir := filepath.Dir(s.path); dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return fmt.Errorf("create settings directory: %w", err)
			}
		}
		raw, err := json.MarshalIndent(next, "", "  ")
		if err != nil {
			return fmt.Errorf("encode settings: %w", err)
		}
		temp := s.path + ".tmp"
		if err := os.WriteFile(temp, raw, 0o600); err != nil {
			return fmt.Errorf("write settings: %w", err)
		}
		if err := os.Rename(temp, s.path); err != nil {
			_ = os.Remove(temp)
			return fmt.Errorf("replace settings: %w", err)
		}
	}
	s.current = next
	s.updated = time.Now()
	return nil
}

func validateSettings(s Settings) error {
	if s.DefaultModel == "" {
		return fmt.Errorf("默认模型不能为空")
	}
	if len(s.DefaultModel) > 200 {
		return fmt.Errorf("默认模型过长")
	}
	if len(s.ModelsHidden) > 2000 {
		return fmt.Errorf("隐藏模型列表过长")
	}
	if err := validateRange("单账号并发上限", s.MaxConcurrent, 1, 10); err != nil {
		return err
	}
	if err := validateRange("排队重试间隔", s.QueueRetrySeconds, 1, 300); err != nil {
		return err
	}
	if err := validateRange("排队最大尝试", s.QueueMaxAttempts, 1, 180); err != nil {
		return err
	}
	if err := validateRange("轮询间隔", s.PollMinutes, 1, 1440); err != nil {
		return err
	}
	if err := validateRange("Token 提前刷新", s.RefreshSkewMinutes, 1, 720); err != nil {
		return err
	}
	if err := validateRange("保活心跳间隔", s.KeepaliveIntervalMinutes, 1, 1440); err != nil {
		return err
	}
	if err := validateRange("保活空闲窗口", s.KeepaliveWindowMinutes, 1, 1440); err != nil {
		return err
	}
	return nil
}

func validateRange(label string, value, minimum, maximum int) error {
	if value < minimum || value > maximum {
		return fmt.Errorf("%s 需在 %d 到 %d 之间", label, minimum, maximum)
	}
	return nil
}

func fallbackSettings(cfg Config) Settings {
	settings := DefaultSettings()
	if cfg.DefaultModel != "" {
		settings.DefaultModel = cfg.DefaultModel
	}
	if cfg.QueueRetryDelay > 0 {
		settings.QueueRetrySeconds = int(cfg.QueueRetryDelay / time.Second)
	}
	if cfg.QueueMaxAttempts > 0 {
		settings.QueueMaxAttempts = cfg.QueueMaxAttempts
	}
	for key, value := range cfg.WatchInfo {
		switch key {
		case "max_concurrent":
			if n, ok := value.(int); ok && n > 0 {
				settings.MaxConcurrent = n
			}
		case "enabled":
			if enabled, ok := value.(bool); ok {
				settings.WatchEnabled = enabled
			}
		case "credits_auto_claim":
			if enabled, ok := value.(bool); ok {
				settings.CreditsAutoClaim = enabled
			}
		case "poll_minutes":
			if n, ok := value.(int); ok && n > 0 {
				settings.PollMinutes = n
			}
		case "refresh_skew_minutes":
			if n, ok := value.(int); ok && n > 0 {
				settings.RefreshSkewMinutes = n
			}
		case "keepalive_interval":
			if n, ok := value.(int); ok && n > 0 {
				settings.KeepaliveIntervalMinutes = n
			}
		}
	}
	return settings
}

func (h *Handler) adminSettingsGet(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Settings == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "message": "settings unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"settings": h.cfg.Settings.Get(),
	})
}

func (h *Handler) adminSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Settings == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "message": "settings unavailable"})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	defer r.Body.Close()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "read body: " + err.Error()})
		return
	}
	var settings Settings
	if err := json.Unmarshal(raw, &settings); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "bad json: " + err.Error()})
		return
	}
	h.settingsMu.Lock()
	if err := h.cfg.Settings.Update(settings); err != nil {
		h.settingsMu.Unlock()
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	saved := h.cfg.Settings.Get()
	h.applySettings(saved)
	h.settingsMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "message": "设置已保存并生效", "settings": saved,
	})
}

func (h *Handler) applySettings(settings Settings) {
	h.cfgMu.Lock()
	h.cfg.DefaultModel = settings.DefaultModel
	h.cfg.QueueRetryDelay = time.Duration(settings.QueueRetrySeconds) * time.Second
	h.cfg.QueueMaxAttempts = settings.QueueMaxAttempts
	if h.cfg.WatchInfo != nil {
		// 复制后整体替换：已发出去的快照（adminOverview）不受后续修改影响。
		watch := make(map[string]any, len(h.cfg.WatchInfo)+6)
		for k, v := range h.cfg.WatchInfo {
			watch[k] = v
		}
		watch["enabled"] = settings.WatchEnabled
		watch["credits_auto_claim"] = settings.CreditsAutoClaim
		watch["poll_minutes"] = settings.PollMinutes
		watch["refresh_skew_minutes"] = settings.RefreshSkewMinutes
		watch["keepalive_interval"] = settings.KeepaliveIntervalMinutes
		watch["keepalive_window"] = fmt.Sprintf("%dm", settings.KeepaliveWindowMinutes)
		watch["max_concurrent"] = settings.MaxConcurrent
		h.cfg.WatchInfo = watch
	}
	h.cfgMu.Unlock()
	if h.cfg.Upstream != nil {
		h.cfg.Upstream.SetBenefitAutoClaim(settings.BenefitAutoClaim)
	}
	if h.cfg.Pool != nil {
		h.cfg.Pool.SetRuntimeConfig(
			settings.MaxConcurrent,
			time.Duration(settings.RefreshSkewMinutes)*time.Minute,
			time.Duration(settings.KeepaliveWindowMinutes)*time.Minute,
		)
	}
	if h.cfg.Scheduler != nil {
		h.cfg.Scheduler.UpdateConfig(scheduler.Config{
			Pool:              h.cfg.Pool,
			Enabled:           settings.WatchEnabled,
			CreditsAutoClaim:  settings.CreditsAutoClaim,
			PollInterval:      time.Duration(settings.PollMinutes) * time.Minute,
			RefreshSkew:       time.Duration(settings.RefreshSkewMinutes) * time.Minute,
			KeepaliveInterval: time.Duration(settings.KeepaliveIntervalMinutes) * time.Minute,
		})
	}
}
