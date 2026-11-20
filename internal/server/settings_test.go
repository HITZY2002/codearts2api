package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSettingsGetUpdateAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	store, err := OpenSettings(path, DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{APIKey: testHandlerAPIKey, Settings: store})

	get := httptest.NewRequest(http.MethodGet, "/admin/api/settings", nil)
	get.Header.Set("Authorization", "Bearer "+testHandlerAPIKey)
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, get)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", getRec.Code, getRec.Body.String())
	}
	var current struct {
		Settings Settings `json:"settings"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &current); err != nil {
		t.Fatal(err)
	}
	if current.Settings.DefaultModel == "" || current.Settings.MaxConcurrent <= 0 {
		t.Fatalf("unexpected settings: %#v", current.Settings)
	}

	next := current.Settings
	next.DefaultModel = "GLM-5.3"
	next.MaxConcurrent = 2
	raw, _ := json.Marshal(next)
	update := httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(string(raw)))
	update.Header.Set("Authorization", "Bearer "+testHandlerAPIKey)
	updateRec := httptest.NewRecorder()
	h.ServeHTTP(updateRec, update)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", updateRec.Code, updateRec.Body.String())
	}
	if h.cfg.DefaultModel != "GLM-5.3" || h.cfg.QueueRetryDelay != time.Duration(store.Get().QueueRetrySeconds)*time.Second {
		t.Fatalf("runtime settings not applied: model=%s retry=%s store=%+v", h.cfg.DefaultModel, h.cfg.QueueRetryDelay, store.Get())
	}

	reloaded, err := OpenSettings(path, DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Get().DefaultModel != "GLM-5.3" || reloaded.Get().MaxConcurrent != 2 {
		t.Fatalf("settings not persisted: %#v", reloaded.Get())
	}
}

func TestSettingsUpdateRejectsInvalidRange(t *testing.T) {
	store, err := OpenSettings("", DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{APIKey: testHandlerAPIKey, Settings: store})
	next := store.Get()
	next.MaxConcurrent = 0
	raw, _ := json.Marshal(next)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(string(raw)))
	req.Header.Set("Authorization", "Bearer "+testHandlerAPIKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
