package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"

	"codearts2api/internal/telemetry"
	"codearts2api/internal/upstream"
)

type requestRecorder struct {
	writer       *responseWriter
	started      time.Time
	record       telemetry.RequestLog
	finishReason string
	// responseDone 冻结「响应写完」的时刻：响应后的积分补查不计入请求耗时。
	responseDone time.Time
}

type requestRecorderKey struct{}

type responseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	onWrite     func([]byte)
}

func (w *responseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.onWrite != nil {
		w.onWrite(data)
	}
	return w.ResponseWriter.Write(data)
}

func (w *responseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func newRequestRecorder(w http.ResponseWriter, r *http.Request, store *telemetry.Store) (*requestRecorder, *responseWriter) {
	started := time.Now()
	requestID := randHex(16)
	rec := &requestRecorder{
		writer:  &responseWriter{ResponseWriter: w, status: http.StatusOK},
		started: started,
		record: telemetry.RequestLog{
			RequestID:  requestID,
			CreatedAt:  started.UnixMilli(),
			Endpoint:   r.URL.Path,
			Method:     r.Method,
			ClientIP:   clientIP(r),
			StatusCode: http.StatusOK,
			Stream:     false,
			Attempts:   0,
		},
	}
	w.Header().Set("X-Codearts-Request-Id", requestID)
	return rec, rec.writer
}

func (rec *requestRecorder) enableStreamOutput() {
	if rec == nil || rec.writer.onWrite != nil {
		return
	}
	rec.record.Stream = true
	rec.writer.onWrite = func(data []byte) {
		if rec.record.FirstOutputMs == nil && hasStreamOutput(data) {
			elapsed := time.Since(rec.started).Milliseconds()
			rec.record.FirstOutputMs = &elapsed
		}
	}
}

func (h *Handler) withTelemetry(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec, wrapped := newRequestRecorder(w, r, h.cfg.Telemetry)
		defer rec.finish(h.cfg.Telemetry)
		next(wrapped, r.WithContext(context.WithValue(r.Context(), requestRecorderKey{}, rec)))
	}
}

// StartSnapshotter periodically records account-pool state.
func (h *Handler) StartSnapshotter(ctx context.Context) {
	if h.cfg.Telemetry == nil {
		return
	}
	interval := h.cfg.SnapshotInterval
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				snapshots := make([]telemetry.AccountSnapshot, 0)
				for _, item := range h.cfg.Pool.List() {
					status := "healthy"
					if disabled, _ := item["disabled"].(bool); disabled {
						status = "disabled"
					} else if cooling, _ := item["cooling"].(bool); cooling {
						status = "cooling"
					}
					accountUID, _ := item["uid"].(string)
					accountName, _ := item["nickname"].(string)
					activeConcurrent, _ := item["active_concurrent"].(int)
					maxConcurrent, _ := item["max_concurrent"].(int)
					errCount, _ := item["err_count"].(int)
					expiresAt, _ := item["expires_at"].(string)
					snapshots = append(snapshots, telemetry.AccountSnapshot{
						AccountUID:       accountUID,
						AccountName:      accountName,
						Status:           status,
						ActiveConcurrent: activeConcurrent,
						MaxConcurrent:    maxConcurrent,
						ErrCount:         errCount,
						TokenExpiresAt:   expiresAt,
					})
				}
				_ = h.cfg.Telemetry.RecordSnapshots(snapshots)
			}
		}
	}()
}

func requestRecorderFromContext(ctx context.Context) *requestRecorder {
	rec, _ := ctx.Value(requestRecorderKey{}).(*requestRecorder)
	return rec
}

func (rec *requestRecorder) finish(store *telemetry.Store) {
	rec.record.StatusCode = rec.writer.status
	rec.record.FinishReason = rec.finishReason
	done := rec.responseDone
	if done.IsZero() {
		done = time.Now()
	}
	rec.record.TotalMs = done.Sub(rec.started).Milliseconds()
	if rec.record.ErrorType == "" && rec.record.StatusCode >= 400 {
		rec.record.ErrorType = "http_error"
	}
	if store != nil {
		_ = store.RecordRequest(rec.record)
	}
}

// markResponseDone 在响应体完整写出后调用，冻结耗时统计。
func (rec *requestRecorder) markResponseDone() {
	if rec != nil && rec.responseDone.IsZero() {
		rec.responseDone = time.Now()
	}
}

func hasStreamOutput(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	return hasNonEmptyJSONField(data, []byte(`"content":`)) ||
		hasNonEmptyJSONField(data, []byte(`"reasoning_content":`)) ||
		hasNonEmptyJSONField(data, []byte(`"tool_calls":`))
}

func hasNonEmptyJSONField(data, key []byte) bool {
	for offset := 0; offset < len(data); {
		idx := bytes.Index(data[offset:], key)
		if idx < 0 {
			return false
		}
		rest := bytes.TrimLeft(data[offset+idx+len(key):], " \t")
		if len(rest) > 0 &&
			!bytes.HasPrefix(rest, []byte(`""`)) &&
			!bytes.HasPrefix(rest, []byte(`null`)) &&
			!bytes.HasPrefix(rest, []byte(`[]`)) {
			return true
		}
		offset += idx + len(key)
	}
	return false
}

func isTrustedProxyPeer(host string) bool {
	ip := net.ParseIP(strings.TrimSpace(host))
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func sanitizeClientIP(raw string) string {
	s := strings.TrimSpace(raw)
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if isTrustedProxyPeer(host) {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			if idx := strings.Index(forwarded, ","); idx >= 0 {
				forwarded = forwarded[:idx]
			}
			if ip := sanitizeClientIP(forwarded); ip != "" {
				return ip
			}
		}
		if realIP := sanitizeClientIP(r.Header.Get("X-Real-IP")); realIP != "" {
			return realIP
		}
	}
	return sanitizeClientIP(host)
}

func numericMapValue(data map[string]any, key string) int64 {
	switch value := data[key].(type) {
	case float64:
		return int64(value)
	case int:
		return int64(value)
	case int64:
		return value
	case json.Number:
		parsed, _ := value.Int64()
		return parsed
	default:
		return 0
	}
}

func creditUsageFromMap(data map[string]any) *upstream.CreditUsage {
	if data == nil {
		return nil
	}
	cost := numericMapValue(data, "credit_cost")
	if cost == 0 {
		return nil
	}
	costValue := float64(cost)
	return &upstream.CreditUsage{
		CreditCost:          costValue,
		PromptTokenUsed:     numericMapValue(data, "prompt_token_used"),
		CompletionTokenUsed: numericMapValue(data, "completion_token_used"),
		CachedTokenUsed:     numericMapValue(data, "cached_token_used"),
		TotalTokenUsed:      numericMapValue(data, "total_token_used"),
		RequestCount:        numericMapValue(data, "request_count"),
	}
}

func applyCompletionMetrics(rec *requestRecorder, msgs []upstream.ChatMessage, comp *upstream.RawCompletion, finish string) {
	if rec == nil || comp == nil {
		return
	}
	rec.record.ErrorType = ""
	if finish != "" {
		rec.finishReason = finish
	} else {
		rec.finishReason = comp.Finish
	}
	usage := usageEstimate(msgs, comp)
	rec.record.PromptTokens = numericMapValue(usage, "prompt_tokens")
	rec.record.CompletionTokens = numericMapValue(usage, "completion_tokens")
	rec.record.TotalTokens = numericMapValue(usage, "total_tokens")
	rec.record.UsageEstimated = comp.Usage == nil
	if comp.CreditUsage != nil {
		cost := comp.CreditUsage.CreditCost
		rec.record.CreditCost = &cost
		rec.record.UpstreamMsgID = comp.CreditUsage.MsgID
		rec.record.BillingPromptTokens = comp.CreditUsage.PromptTokenUsed
		rec.record.BillingCompletionTokens = comp.CreditUsage.CompletionTokenUsed
		rec.record.BillingCachedTokens = comp.CreditUsage.CachedTokenUsed
		rec.record.BillingTotalTokens = comp.CreditUsage.TotalTokenUsed
		rec.record.BillingRequestCount = comp.CreditUsage.RequestCount
	}
}
