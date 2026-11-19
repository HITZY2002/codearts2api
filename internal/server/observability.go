package server

import (
	"net/http"
	"strconv"
	"time"

	"codearts2api/internal/telemetry"
)

func timeParam(r *http.Request, key string) (int64, error) {
	return telemetry.ParseTimeParam(r.URL.Query().Get(key))
}

func intParam(r *http.Request, key string, fallback int) int {
	value := r.URL.Query().Get(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func (h *Handler) adminLogs(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Telemetry == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "message": "observability disabled",
		})
		return
	}
	from, err := timeParam(r, "from")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	to, err := timeParam(r, "to")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	query := r.URL.Query()
	streamParam := query.Get("stream")
	var stream *bool
	if streamParam != "" {
		streamValue := streamParam == "true" || streamParam == "1"
		stream = &streamValue
	}
	statusCode, _ := strconv.Atoi(query.Get("status_code"))
	items, total, err := h.cfg.Telemetry.ListRequests(telemetry.RequestFilter{
		From:       from,
		To:         to,
		StatusCode: statusCode,
		Stream:     stream,
		Model:      query.Get("model"),
		AccountUID: query.Get("account_uid"),
		ErrorType:  query.Get("error_type"),
		Limit:      intParam(r, "limit", 100),
		Offset:     intParam(r, "offset", 0),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "items": items, "total": total,
		"limit": intParam(r, "limit", 100), "offset": intParam(r, "offset", 0),
	})
}

func (h *Handler) adminEvents(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Telemetry == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "message": "observability disabled",
		})
		return
	}
	from, err := timeParam(r, "from")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	to, err := timeParam(r, "to")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	query := r.URL.Query()
	items, total, err := h.cfg.Telemetry.ListEvents(telemetry.EventFilter{
		From:       from,
		To:         to,
		Kind:       query.Get("kind"),
		Level:      query.Get("level"),
		AccountUID: query.Get("account_uid"),
		Model:      query.Get("model"),
		Limit:      intParam(r, "limit", 100),
		Offset:     intParam(r, "offset", 0),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "items": items, "total": total,
		"limit": intParam(r, "limit", 100), "offset": intParam(r, "offset", 0),
	})
}

func (h *Handler) adminMetricsOverview(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Telemetry == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "message": "observability disabled",
		})
		return
	}
	now := time.Now().UnixMilli()
	from := now - int64(24*time.Hour/time.Millisecond)
	to := now
	var err error
	if raw := r.URL.Query().Get("from"); raw != "" {
		from, err = timeParam(r, "from")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
			return
		}
	}
	if raw := r.URL.Query().Get("to"); raw != "" {
		to, err = timeParam(r, "to")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
			return
		}
	}
	overview, err := h.cfg.Telemetry.MetricsOverview(from, to)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "from": from, "to": to, "metrics": overview,
	})
}

func (h *Handler) adminMetricsSeries(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Telemetry == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "message": "observability disabled",
		})
		return
	}
	now := time.Now().UnixMilli()
	from := now - int64(24*time.Hour/time.Millisecond)
	to := now
	var err error
	if raw := r.URL.Query().Get("from"); raw != "" {
		from, err = timeParam(r, "from")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
			return
		}
	}
	if raw := r.URL.Query().Get("to"); raw != "" {
		to, err = timeParam(r, "to")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
			return
		}
	}
	interval := r.URL.Query().Get("interval")
	if interval == "" {
		interval = "hour"
	}
	series, err := h.cfg.Telemetry.MetricsSeries(from, to, interval)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "from": from, "to": to, "interval": interval, "items": series,
	})
}

func (h *Handler) adminObservabilityPurge(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Telemetry == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "message": "observability disabled",
		})
		return
	}
	stats, err := h.cfg.Telemetry.Purge(
		h.cfg.ObservabilityRetentionDays,
		h.cfg.ObservabilityMaxRows,
		h.cfg.ObservabilityMaxDBBytes,
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "message": "已清理", "stats": stats,
	})
}
