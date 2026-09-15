package pool

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/upstream"
)

func TestAddAccountInitializesConcurrency(t *testing.T) {
	p, err := New(nil, Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "verifier")
	p.AddAccount(a)

	if !p.AcquireLockWait("u1", 0) {
		t.Fatal("newly added account did not receive a concurrency slot")
	}
	p.ReleaseLock("u1")
}

func TestSyncToDirInitializesConcurrency(t *testing.T) {
	p, err := New(nil, Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "verifier")
	p.SyncToDir([]*auth.Auth{a})

	if !p.AcquireLockWait("u1", 0) {
		t.Fatal("synced account did not receive a concurrency slot")
	}
	p.ReleaseLock("u1")
}

func TestValidateKeepsAccountEnabledAfterRetryableRefreshFailure(t *testing.T) {
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporary upstream outage", http.StatusBadGateway)
	}))
	defer sts.Close()
	dpopPrivateJWK, err := upstream.NewDPoPPrivateJWK()
	if err != nil {
		t.Fatal(err)
	}
	loginCfg := upstream.DefaultLoginConfig()
	loginCfg.STSHost = sts.URL
	a := auth.New(
		"u-refresh", "user", "domain", "token", "ak", "sk",
		time.Now().Add(30*time.Minute).Format(time.RFC3339), "refresh", "verifier",
		map[string]string(dpopPrivateJWK),
	)
	p, err := New([]*auth.Auth{a}, Config{RefreshSkew: time.Hour, LoginConfig: loginCfg}, "")
	if err != nil {
		t.Fatal(err)
	}
	ok, refreshErr := p.Validate(p.Accounts()[0])
	if ok || refreshErr == nil {
		t.Fatalf("validate=(ok=%t, err=%v), want retryable refresh failure", ok, refreshErr)
	}
	status := p.List()[0]
	if disabled, _ := status["disabled"].(bool); disabled {
		t.Fatalf("retryable refresh failure permanently disabled account: %#v", status)
	}
}
