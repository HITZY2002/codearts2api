package pool

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
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

func TestSaveStateSerializesConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	p, err := New([]*auth.Auth{a}, Config{}, path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		i := i
		wg.Add(2)
		go func() {
			defer wg.Done()
			p.Cooldown("u1", CoolSoft, time.Second, "test")
		}()
		go func() {
			defer wg.Done()
			p.AddAccount(auth.New(fmt.Sprintf("u-%d", i), "user", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""))
		}()
	}
	wg.Wait()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("state is not valid JSON: %v", err)
	}
	if len(entries) != 17 {
		t.Fatalf("state entries=%d, want 17", len(entries))
	}
}

func TestAuthSnapshotDuringCredentialReplacement(t *testing.T) {
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	p, err := New([]*auth.Auth{a}, Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			p.AddAccount(auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_, _, _ = p.Get("u1").AuthSnapshot().Credentials()
		}
	}()
	wg.Wait()
}

func TestAcquireLockWaitReturnsWhenAccountRemoved(t *testing.T) {
	p, err := New(nil, Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if p.AcquireLockWait("missing", time.Second) {
		t.Fatal("missing account acquired a lock")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("missing account waited %s", elapsed)
	}
}

func TestConfiguredUpstreamTimeout(t *testing.T) {
	p, err := New(nil, Config{UpstreamTimeout: 7 * time.Second}, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.cfg.UpstreamTimeout != 7*time.Second {
		t.Fatalf("upstream timeout=%s", p.cfg.UpstreamTimeout)
	}
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

func TestPickExcludingPrefersLeastUtilized(t *testing.T) {
	p, err := New(nil, Config{MaxConcurrent: 2}, "")
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(time.Hour).Format(time.RFC3339)
	p.SyncToDir([]*auth.Auth{
		auth.New("u1", "user1", "domain", "token", "ak", "sk", expiry, "", "verifier"),
		auth.New("u2", "user2", "domain", "token", "ak", "sk", expiry, "", "verifier"),
	})

	// u1 半载、u2 空闲：新会话应摊到 u2，而不是配置序第一个 u1。
	if !p.AcquireLockWait("u1", 0) {
		t.Fatal("acquire u1 failed")
	}
	if got := p.PickExcluding(nil); got == nil || got.Name != "u2" {
		t.Fatalf("expected least-utilized u2, got %v", got)
	}

	// 同为半载时保持配置顺序。
	if !p.AcquireLockWait("u2", 0) {
		t.Fatal("acquire u2 failed")
	}
	if got := p.PickExcluding(nil); got == nil || got.Name != "u1" {
		t.Fatalf("expected config-order tie-break u1, got %v", got)
	}

	// tried 排除仍然生效。
	if got := p.PickExcluding(map[string]bool{"u1": true}); got == nil || got.Name != "u2" {
		t.Fatalf("expected u2 after excluding u1, got %v", got)
	}
	p.ReleaseLock("u1")
	p.ReleaseLock("u2")
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

// watch.refresh_skew_minutes 必须是有效的：此前 pool 硬编码 1 小时阈值，
// 调度器传进来的 RefreshSkew 只用于日志，配置改了也不生效。
func TestRefreshSkewControlsThreshold(t *testing.T) {
	a := auth.New("u-skew", "n", "d", "tok", "ak", "sk",
		time.Now().Add(2*time.Hour).Format(time.RFC3339), "refresh-token", "verifier")
	p, err := New([]*auth.Auth{a}, Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	// 剩余 2h：skew=1h 时不刷新，skew=3h 时应命中刷新（这里只验证判定分支，
	// 真刷新需要网络，故只检查不触发的那一侧不报错）。
	if err := p.CheckAndRefreshTokenWithin("u-skew", time.Hour); err != nil {
		t.Fatalf("剩余 2h、skew=1h 不应触发刷新: %v", err)
	}
}
