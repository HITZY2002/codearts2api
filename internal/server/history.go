package server

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CallRecord 一次推理调用的审计记录，面板「调用历史」直接展示。
type CallRecord struct {
	Time             time.Time `json:"time"`
	Model            string    `json:"model"`
	Account          string    `json:"account"`
	Stream           bool      `json:"stream"`
	TTFTMs           int       `json:"ttft_ms"`
	TotalMs          int       `json:"total_ms"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	CachedTokens     int       `json:"cached_tokens"`
	Status           int       `json:"status"`
	Error            string    `json:"error,omitempty"`
}

// History 保存最近 N 条调用记录（内存环形缓冲），并逐行追加到 JSONL 供外部审计。
type History struct {
	mu    sync.Mutex
	ring  []CallRecord
	limit int
	file  *os.File
}

// NewHistory path 为空时只保留内存记录。
func NewHistory(path string, limit int) *History {
	if limit <= 0 {
		limit = 500
	}
	h := &History{limit: limit}
	if path == "" {
		return h
	}
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	h.load(path)
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		h.file = f
	}
	return h
}

// load 只恢复最后 limit 条，避免历史文件无限增长后吃内存。
func (h *History) load(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var rec CallRecord
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		h.ring = append(h.ring, rec)
		if len(h.ring) > h.limit {
			h.ring = h.ring[len(h.ring)-h.limit:]
		}
	}
}

// Add 追加一条记录。
func (h *History) Add(rec CallRecord) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ring = append(h.ring, rec)
	if len(h.ring) > h.limit {
		h.ring = h.ring[len(h.ring)-h.limit:]
	}
	if h.file != nil {
		if b, err := json.Marshal(rec); err == nil {
			_, _ = h.file.Write(append(b, '\n'))
		}
	}
}

// List 返回最新在前的记录，limit<=0 时按 100 条。
func (h *History) List(limit int) []CallRecord {
	if limit <= 0 || limit > h.limit {
		limit = 100
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]CallRecord, 0, limit)
	for i := len(h.ring) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, h.ring[i])
	}
	return out
}

// Summary 近 N 条的聚合指标：调用数、平均首字、命中率（cached/prompt，均为上游 usage）。
func (h *History) Summary(limit int) map[string]any {
	recs := h.List(limit)
	var ttft, hit float64
	var cached, prompt, completion int
	for _, r := range recs {
		ttft += float64(r.TTFTMs)
		cached += r.CachedTokens
		prompt += r.PromptTokens
		completion += r.CompletionTokens
	}
	n := float64(len(recs))
	out := map[string]any{"count": len(recs), "prompt_tokens": prompt, "completion_tokens": completion, "cached_tokens": cached}
	if n > 0 {
		out["avg_ttft_ms"] = int(ttft / n)
	}
	if prompt > 0 {
		hit = float64(cached) / float64(prompt) * 100
		out["hit_rate"] = float64(int(hit*10)) / 10
	}
	return out
}
