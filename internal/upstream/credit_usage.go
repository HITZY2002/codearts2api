package upstream

import (
	"context"
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const msgIDAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// msgIDClock mirrors the global monotonic clock state in agent-kernel.exe:
// IDs generated within the same millisecond carry a 12-bit sequence (1..4095);
// when the sequence overflows, the millisecond is bumped by one.
var msgIDClock struct {
	sync.Mutex
	lastMS int64
	seq    uint16
}

// NewUpstreamMsgID reproduces the official agent-kernel message ID generator
// (verified by reverse engineering agent-kernel.exe):
//
//	msg_ + 12 lowercase hex of ((unix_ms << 12) | seq12) + 14 base62 chars.
//
// The suffix is the first 14 bytes of a UUIDv4, each byte mapped through
// alphabet[b % 62]. The trailing 001/002 seen in captured IDs is the
// per-millisecond sequence number, not a client-kind marker.
func NewUpstreamMsgID(now time.Time) string {
	ms := now.UnixMilli()
	msgIDClock.Lock()
	if ms <= msgIDClock.lastMS {
		if msgIDClock.seq >= 0xFFF {
			msgIDClock.lastMS++
			msgIDClock.seq = 1
		} else {
			msgIDClock.seq++
		}
	} else {
		msgIDClock.lastMS = ms
		msgIDClock.seq = 1
	}
	stamp := ((uint64(msgIDClock.lastMS) << 12) & 0xFFFFFFFFF000) | uint64(msgIDClock.seq)
	msgIDClock.Unlock()

	var uuid [16]byte
	if _, err := crand.Read(uuid[:]); err != nil {
		for i := range uuid {
			uuid[i] = byte(now.UnixNano() % 251)
		}
	}
	uuid[6] = (uuid[6] & 0x0F) | 0x40 // UUID version 4
	uuid[8] = (uuid[8] & 0x3F) | 0x80 // RFC 4122 variant
	suffix := make([]byte, 14)
	for i := 0; i < 14; i++ {
		suffix[i] = msgIDAlphabet[uuid[i]%62]
	}
	return fmt.Sprintf("msg_%012x%s", stamp, string(suffix))
}

// FetchCreditUsage reads the same per-turn credit usage used by the desktop client.
func (c *Client) FetchCreditUsage(ctx context.Context, msgID string, cred SignCredential) (*CreditUsage, error) {
	if msgID == "" {
		return nil, fmt.Errorf("message id is empty")
	}
	query := url.Values{"type": {"msg"}, "id": {msgID}}.Encode()
	raw, err := c.creditRequest(ctx, http.MethodGet, EpCreditUsage+"?"+query, nil, cred)
	if err != nil {
		return nil, err
	}
	var data struct {
		CreditCost          *float64 `json:"creditCost"`
		CreditCostAlt       *float64 `json:"credit_cost"`
		CacheHitRatio       *float64 `json:"cacheHitRatio"`
		CacheHitRatioAlt    *float64 `json:"cache_hit_ratio"`
		PromptTokenUsed     int64    `json:"promptTokenUsed"`
		PromptTokenUsedAlt  int64    `json:"prompt_token_used"`
		CompletionTokenUsed int64    `json:"completionTokenUsed"`
		CompletionTokenAlt  int64    `json:"completion_token_used"`
		CachedTokenUsed     int64    `json:"cachedTokenUsed"`
		CachedTokenAlt      int64    `json:"cached_token_used"`
		TotalTokenUsed      int64    `json:"totalTokenUsed"`
		TotalTokenAlt       int64    `json:"total_token_used"`
		RequestCount        int64    `json:"requestCount"`
		RequestCountAlt     int64    `json:"request_count"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("parse credit usage: %w", err)
	}
	out := &CreditUsage{
		PromptTokenUsed:     firstInt64(data.PromptTokenUsed, data.PromptTokenUsedAlt),
		CompletionTokenUsed: firstInt64(data.CompletionTokenUsed, data.CompletionTokenAlt),
		CachedTokenUsed:     firstInt64(data.CachedTokenUsed, data.CachedTokenAlt),
		TotalTokenUsed:      firstInt64(data.TotalTokenUsed, data.TotalTokenAlt),
		RequestCount:        firstInt64(data.RequestCount, data.RequestCountAlt),
		MsgID:               msgID,
	}
	if data.CreditCost != nil {
		out.CreditCost = *data.CreditCost
	} else if data.CreditCostAlt != nil {
		out.CreditCost = *data.CreditCostAlt
	}
	if data.CacheHitRatio != nil {
		out.CacheHitRatio = *data.CacheHitRatio
	} else if data.CacheHitRatioAlt != nil {
		out.CacheHitRatio = *data.CacheHitRatioAlt
	}
	return out, nil
}

func firstInt64(values ...int64) int64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}
