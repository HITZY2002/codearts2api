package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

type failingCheckinClient struct{ calls int }

func (f *failingCheckinClient) Checkin(context.Context, upstream.SignCredential) (upstream.CheckinResult, error) {
	f.calls++
	return upstream.CheckinResult{}, errors.New("temporary activity failure")
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
