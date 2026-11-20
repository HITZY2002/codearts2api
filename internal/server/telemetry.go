package server

import (
	"encoding/json"
	"math"
	"net/http"
	"sync"
	"time"
)

// telemetryWriter 记录首次写出时间（= 首字延迟）与最终状态码。
type telemetryWriter struct {
	http.ResponseWriter
	mu    sync.Mutex
	first time.Time
	code  int
}

func (t *telemetryWriter) markFirst() {
	t.mu.Lock()
	if t.first.IsZero() {
		t.first = time.Now()
	}
	t.mu.Unlock()
}

func (t *telemetryWriter) WriteHeader(code int) {
	t.mu.Lock()
	if t.code == 0 {
		t.code = code
	}
	t.mu.Unlock()
	t.ResponseWriter.WriteHeader(code)
}

func (t *telemetryWriter) Write(b []byte) (int, error) {
	t.markFirst()
	return t.ResponseWriter.Write(b)
}

// Flush 让 SSE 立刻下发，同时把首个字节当作首字时间。
func (t *telemetryWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Status 未显式写过 header 时按 200 计。
func (t *telemetryWriter) Status() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.code == 0 {
		return http.StatusOK
	}
	return t.code
}

// FirstDuration 首字耗时；从未写出时返回总耗时。
func (t *telemetryWriter) FirstDuration(start time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.first.IsZero() {
		return time.Since(start)
	}
	return t.first.Sub(start)
}

func num(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return int(x)
	case float32:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return int(n)
		}
	}
	return 0
}

// usageNumbers 取 prompt / completion / 缓存命中 tokens。
func usageNumbers(usage map[string]any) (prompt, completion, cached int) {
	prompt = num(usage, "prompt_tokens")
	completion = num(usage, "completion_tokens")
	cached = num(usage, "cached_tokens")
	if d, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		cached = num(d, "cached_tokens")
	}
	return
}

func round4(v float64) float64 {
	return math.Round(v*10000) / 10000
}

// recordCall 落一条调用历史，并把上游 usage 的真实 token 数累加到账号。
// 不做任何本地估算：没有 usage 就只记状态，不记 token。
func (h *Handler) recordCall(model, account string, stream bool, tw *telemetryWriter, start time.Time, usage map[string]any) {
	prompt, completion, cached := usageNumbers(usage)
	rec := CallRecord{
		Time:             start,
		Model:            model,
		Account:          account,
		Stream:           stream,
		TTFTMs:           int(tw.FirstDuration(start) / time.Millisecond),
		TotalMs:          int(time.Since(start) / time.Millisecond),
		PromptTokens:     prompt,
		CompletionTokens: completion,
		CachedTokens:     cached,
		Status:           tw.Status(),
	}
	if rec.Status < 400 && account != "" {
		h.cfg.Pool.AddUsage(account, prompt, completion, cached)
	}
	if h.hist != nil {
		h.hist.Add(rec)
	}
}
