package pool

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/upstream"
)

func rotationTestAuths(names ...string) []*auth.Auth {
	out := make([]*auth.Auth, 0, len(names))
	for _, n := range names {
		out = append(out, &auth.Auth{UserID: n})
	}
	return out
}

func findState(t *testing.T, p *Pool, name string) map[string]any {
	t.Helper()
	for _, row := range p.List() {
		if row["name"] == name {
			return row
		}
	}
	t.Fatalf("账号 %s 不在池中", name)
	return nil
}

// 并发锁上限：达到 maxConcurrent 后 AcquireLock / AcquireLockWait 都必须拒绝，
// 释放一个槽位后即可再获取一个。
func TestAcquireLockEnforcesCap(t *testing.T) {
	p, err := New(rotationTestAuths("a"), Config{MaxConcurrent: 2}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !p.AcquireLock("a") || !p.AcquireLock("a") {
		t.Fatal("前两次获取应成功")
	}
	if p.AcquireLock("a") {
		t.Fatal("达到上限后 AcquireLock 应拒绝")
	}
	if p.AcquireLockWait("a", 200*time.Millisecond) {
		t.Fatal("达到上限后 AcquireLockWait 超时应返回 false")
	}
	p.ReleaseLock("a")
	if !p.AcquireLock("a") {
		t.Fatal("释放一个槽位后应能再获取")
	}
	row := findState(t, p, "a")
	if got := row["active_concurrent"].(int); got != 2 {
		t.Fatalf("active_concurrent=%d, want 2", got)
	}
}

// ReleaseLock 在无持锁时调用必须把计数钳在 0，不允许出现负数。
func TestReleaseLockFloorsAtZero(t *testing.T) {
	p, err := New(rotationTestAuths("a"), Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.ReleaseLock("a")
	p.ReleaseLock("a")
	if got := findState(t, p, "a")["active_concurrent"].(int); got != 0 {
		t.Fatalf("active_concurrent=%d, want 0（不得为负）", got)
	}
	// 钳零后仍能正常获取。
	if !p.AcquireLock("a") {
		t.Fatal("钳零后应能正常获取槽位")
	}
}

// NoteError 达到阈值触发冷却；NoteSuccess 只清错误计数、不解冷却；
// ClearCooldown 显式解冷却。
func TestNoteErrorThresholdCooldownAndNoteSuccess(t *testing.T) {
	p, err := New(rotationTestAuths("a"), Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.NoteError("a", 2, 10*time.Minute)
	if !p.Healthy("a") {
		t.Fatal("未达阈值不应冷却")
	}
	p.NoteError("a", 2, 10*time.Minute)
	if p.Healthy("a") {
		t.Fatal("达到阈值应进入冷却")
	}
	p.NoteSuccess("a")
	if got := findState(t, p, "a")["err_count"].(int); got != 0 {
		t.Fatalf("NoteSuccess 应清零错误计数，got=%d", got)
	}
	if p.Healthy("a") {
		t.Fatal("NoteSuccess 不应解除冷却")
	}
	p.ClearCooldown("a")
	if !p.Healthy("a") {
		t.Fatal("ClearCooldown 应解除冷却")
	}
}

// 冷却/禁用/错误计数持久化：进程重建后状态恢复。
func TestPoolStateRoundTripAcrossRebuild(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	p1, err := New(rotationTestAuths("a", "b"), Config{}, stateFile)
	if err != nil {
		t.Fatal(err)
	}
	p1.Cooldown("a", CoolErr, 10*time.Minute, "upstream 500")
	p1.Disable("b", "refresh_token expired")
	p1.NoteError("a", 100, 10*time.Minute)

	p2, err := New(rotationTestAuths("a", "b"), Config{}, stateFile)
	if err != nil {
		t.Fatal(err)
	}
	rowA := findState(t, p2, "a")
	if rowA["cooling"] != true {
		t.Fatalf("重建后 a 应仍处于冷却，got=%#v", rowA)
	}
	if rowA["err_count"].(int) != 1 {
		t.Fatalf("重建后 a 的错误计数应恢复，got=%d", rowA["err_count"].(int))
	}
	if rowA["last_error"] != "upstream 500" {
		t.Fatalf("重建后 a 的最后错误应恢复，got=%#v", rowA["last_error"])
	}
	if p2.Healthy("a") {
		t.Fatal("重建后 a 不应被视为健康")
	}
	rowB := findState(t, p2, "b")
	if rowB["disabled"] != true || rowB["reason"] != "refresh_token expired" {
		t.Fatalf("重建后 b 应保持禁用状态，got=%#v", rowB)
	}
}

// PickExcluding 尊重 tried 排除集，并把请求摊到利用率最低的账号。
func TestPickExcludingRespectsTriedAndUtilization(t *testing.T) {
	p, err := New(rotationTestAuths("a", "b"), Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.AcquireLock("a") // a 利用率 1/1
	if got := p.PickExcluding(map[string]bool{"a": true}); got == nil || got.Name != "b" {
		t.Fatalf("a 被排除后应挑到 b，got=%v", got)
	}
	if got := p.PickExcluding(nil); got == nil || got.Name != "b" {
		t.Fatalf("a 满载时应挑到空闲的 b，got=%v", got)
	}
	if got := p.PickExcluding(map[string]bool{"a": true, "b": true}); got != nil {
		t.Fatalf("全部被排除时应返回 nil，got=%v", got)
	}
}

// refreshTestPool 构建一个 STS 刷新必定成功的池（token 30 分钟后到期、
// skew 1 小时 → Validate 必触发刷新）。
func refreshTestPool(t *testing.T) *Pool {
	t.Helper()
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"new-tok","refresh_token":"refresh-2",`+
			`"credentials":{"access_key_id":"ak2","secret_access_key":"sk2","security_token":"st2",`+
			`"expiration":"`+time.Now().Add(2*time.Hour).UTC().Format(time.RFC3339)+`"}}`)
	}))
	t.Cleanup(sts.Close)
	dpopPrivateJWK, err := upstream.NewDPoPPrivateJWK()
	if err != nil {
		t.Fatal(err)
	}
	loginCfg := upstream.DefaultLoginConfig()
	loginCfg.STSHost = sts.URL
	a := auth.New(
		"u-refreshable", "user", "domain", "token", "ak", "sk",
		time.Now().Add(30*time.Minute).Format(time.RFC3339), "refresh", "verifier",
		map[string]string(dpopPrivateJWK),
	)
	if err := a.SetPathAndSave(filepath.Join(t.TempDir(), "u-refreshable.json")); err != nil {
		t.Fatal(err)
	}
	p, err := New([]*auth.Auth{a}, Config{RefreshSkew: time.Hour, LoginConfig: loginCfg}, "")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// 管理员手动禁用的账号：token 刷新成功也不得被静默解禁（否则 watchdog /
// 余额查询路径的 Validate 会把手动下线的账号复活）。
func TestManualDisableSurvivesTokenRefresh(t *testing.T) {
	p := refreshTestPool(t)
	if !p.DisableManual("u-refreshable", "ops") {
		t.Fatal("手动禁用应成功")
	}
	ok, verr := p.Validate(p.Accounts()[0])
	if !ok || verr != nil {
		t.Fatalf("validate 应成功（刷新成功），got ok=%t err=%v", ok, verr)
	}
	if p.Enabled("u-refreshable") {
		t.Fatal("手动禁用不应被刷新解除")
	}
}

// 自动禁用（非手动，如 401）的账号：刷新成功后应自愈解禁。
func TestAutoDisableRevivedByTokenRefresh(t *testing.T) {
	p := refreshTestPool(t)
	if !p.Disable("u-refreshable", "401 unauthorized") {
		t.Fatal("自动禁用应成功")
	}
	ok, verr := p.Validate(p.Accounts()[0])
	if !ok || verr != nil {
		t.Fatalf("validate 应成功（刷新成功），got ok=%t err=%v", ok, verr)
	}
	if !p.Enabled("u-refreshable") {
		t.Fatal("自动禁用应在刷新成功后解禁")
	}
}

// 排队等待必须感知 ctx 取消：客户端断开后立即返回，而不是空等满超时。
func TestAcquireLockWaitCtxReturnsOnCancel(t *testing.T) {
	p, err := New(rotationTestAuths("a"), Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !p.AcquireLock("a") {
		t.Fatal("占住唯一槽位")
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if p.AcquireLockWaitCtx(ctx, "a", 30*time.Second) {
		t.Fatal("ctx 取消后应返回 false")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("ctx 取消应立即放弃排队，耗时 %s", elapsed)
	}
}
