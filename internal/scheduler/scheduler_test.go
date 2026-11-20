package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

type failingCheckinClient struct {
	mu    sync.Mutex
	calls int
}

func (f *failingCheckinClient) Checkin(context.Context, upstream.SignCredential) (upstream.CheckinResult, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return upstream.CheckinResult{}, errors.New("temporary activity failure")
}

func (f *failingCheckinClient) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type completedCheckinClient struct{ calls int }

func (c *completedCheckinClient) Checkin(context.Context, upstream.SignCredential) (upstream.CheckinResult, error) {
	c.calls++
	return upstream.CheckinResult{OK: true, Skipped: true, Message: "今日已签到"}, nil
}

type readOnlyKeepaliveClient struct{ calls int }

func (c *readOnlyKeepaliveClient) FetchCreditCampaigns(context.Context, upstream.SignCredential) ([]upstream.CreditCampaign, error) {
	c.calls++
	return []upstream.CreditCampaign{}, nil
}

func TestAutoCheckinOptOutAndRetry(t *testing.T) {
	c := &failingCheckinClient{}
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{a}, pool.Config{CheckinClient: c}, "")
	if err != nil {
		t.Fatal(err)
	}
	New(Config{Pool: p}).Tick(context.Background())
	if c.calls != 0 {
		t.Fatal("auto claim ran while opted out")
	}
	s := New(Config{Pool: p, CreditsAutoClaim: true})
	s.Tick(context.Background())
	s.Tick(context.Background())
	if c.calls != 2 || !p.Healthy("u1") {
		t.Fatalf("retry failed or account damaged: calls=%d", c.calls)
	}
	p.Disable("u1", "manual")
	s.Tick(context.Background())
	if c.calls != 2 {
		t.Fatal("disabled account was claimed")
	}
}

func TestAutoCheckinRunsOncePerDay(t *testing.T) {
	client := &completedCheckinClient{}
	account := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{account}, pool.Config{CheckinClient: client}, "")
	if err != nil {
		t.Fatal(err)
	}
	scheduler := New(Config{Pool: p, CreditsAutoClaim: true})
	scheduler.Tick(context.Background())
	scheduler.Tick(context.Background())
	if client.calls != 1 {
		t.Fatalf("same-day checkin calls=%d, want 1", client.calls)
	}
}

func TestKeepaliveTickUsesRealReadOnlyRequest(t *testing.T) {
	client := &readOnlyKeepaliveClient{}
	account := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{account}, pool.Config{
		KeepaliveClient: client, KeepaliveWindow: time.Nanosecond,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	New(Config{Pool: p}).KeepaliveTick(context.Background())
	if client.calls != 1 {
		t.Fatalf("keepalive upstream calls=%d, want 1", client.calls)
	}
	if p.NeedKeepalive("u1") {
		t.Fatal("successful upstream keepalive did not refresh the heartbeat")
	}
}

// TestRunAppliesConfigChangesLive 验证运行中开关与周期热更新：
// 启动时关闭 → 打开后立即补一轮；周期缩短后按新周期触发（旧 ticker 不重建会挂 1h）。
func TestRunAppliesConfigChangesLive(t *testing.T) {
	client := &failingCheckinClient{}
	account := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{account}, pool.Config{CheckinClient: client}, "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{Pool: p, Enabled: false, PollInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	time.Sleep(80 * time.Millisecond)
	if client.Calls() != 0 {
		t.Fatalf("disabled watchdog ticked: calls=%d", client.Calls())
	}
	s.UpdateConfig(Config{Pool: p, Enabled: true, CreditsAutoClaim: true, PollInterval: 20 * time.Millisecond})
	deadline := time.Now().Add(3 * time.Second)
	for client.Calls() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("live config change did not take effect: calls=%d", client.Calls())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after context cancel")
	}
}
