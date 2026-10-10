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
