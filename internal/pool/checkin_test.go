package pool

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/upstream"
)

type checkinClientFunc func(context.Context, upstream.SignCredential) (upstream.CheckinResult, error)

func (f checkinClientFunc) Checkin(ctx context.Context, cred upstream.SignCredential) (upstream.CheckinResult, error) {
	return f(ctx, cred)
}

func TestCheckinSerializesAndKeepsInferenceHealthy(t *testing.T) {
	var calls, active, peak atomic.Int32
	client := checkinClientFunc(func(ctx context.Context, cred upstream.SignCredential) (upstream.CheckinResult, error) {
		calls.Add(1)
		n := active.Add(1)
		if n > peak.Load() {
			peak.Store(n)
		}
		defer active.Add(-1)
		if cred.SecurityToken != "token" || cred.AccessKeyID != "ak" {
			t.Error("wrong credential snapshot")
		}
		time.Sleep(10 * time.Millisecond)
		return upstream.CheckinResult{}, errors.New("activity unavailable")
	})
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	p, err := New([]*auth.Auth{a}, Config{ErrThreshold: 1, CheckinClient: client}, "")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = p.Checkin(context.Background(), "u1") }()
	}
	wg.Wait()
	if calls.Load() != 3 || peak.Load() != 1 {
		t.Fatalf("calls=%d peak=%d", calls.Load(), peak.Load())
	}
	st := p.List()[0]
	if !p.Healthy("u1") || st["err_count"] != 0 || st["active_concurrent"] != 0 {
		t.Fatalf("activity error damaged inference: %+v", st)
	}
	if st["checkin"].(*CheckinStatus).OK {
		t.Fatal("failed checkin reported as success")
	}
	p.Disable("u1", "manual")
	if _, err := p.Checkin(context.Background(), "u1"); err == nil || calls.Load() != 3 {
		t.Fatal("disabled account was claimed")
	}
}

func TestCheckinAccumulatesCredits(t *testing.T) {
	client := checkinClientFunc(func(ctx context.Context, cred upstream.SignCredential) (upstream.CheckinResult, error) {
		return upstream.CheckinResult{OK: true, Credits: 4000, Message: "领取成功（4000 积分）"}, nil
	})
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	p, err := New([]*auth.Auth{a}, Config{ErrThreshold: 1, CheckinClient: client}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Checkin(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	st := p.List()[0]
	if got := st["credits"]; got != 4000.0 {
		t.Fatalf("credits after first claim = %v, want 4000", got)
	}
	if cs := st["checkin"].(*CheckinStatus); cs.Credits != 4000 {
		t.Fatalf("checkin credits = %v, want 4000", cs.Credits)
	}
	if _, _, _, _, credits := p.Stats(); credits != 4000 {
		t.Fatalf("stats credits = %d, want 4000", credits)
	}
	// 同一天重复签到不得重复累加
	if _, err := p.Checkin(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, credits := p.Stats(); credits != 4000 {
		t.Fatalf("stats credits after same-day repeat = %d, want 4000", credits)
	}
	// 跨天后可以再次累计
	p.accounts[0].creditsDay = "2000-01-01"
	if _, err := p.Checkin(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, credits := p.Stats(); credits != 8000 {
		t.Fatalf("stats credits after next-day claim = %d, want 8000", credits)
	}
}
