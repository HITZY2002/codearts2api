package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

// 每个账号一个独立假上游并带请求计数，便于断言「哪个账号实际服务了请求」
// 以及「每个上游被打了几次」。
func stickyRotationHarness(t *testing.T, upstreamA, upstreamB http.HandlerFunc) (*Handler, *pool.Pool, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	var callsA, callsB atomic.Int64
	counting := func(counter *atomic.Int64, next http.HandlerFunc) http.HandlerFunc {
		if next == nil {
			next = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}
		}
		return func(w http.ResponseWriter, r *http.Request) {
			counter.Add(1)
			next(w, r)
		}
	}
	serverA := httptest.NewServer(http.HandlerFunc(counting(&callsA, upstreamA)))
	t.Cleanup(serverA.Close)
	serverB := httptest.NewServer(http.HandlerFunc(counting(&callsB, upstreamB)))
	t.Cleanup(serverB.Close)

	auths := []*auth.Auth{
		auth.New(t.Name()+"-a", "tester", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""),
		auth.New(t.Name()+"-b", "tester", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""),
	}
	p, err := pool.New(auths, pool.Config{MaxConcurrent: 1, ErrThreshold: 100}, "")
	if err != nil {
		t.Fatal(err)
	}
	useFreshTestCatalogs(p)
	// Accounts() 按注册顺序返回：[...-a, ...-b]，与 serverA/serverB 一一对应。
	accts := p.Accounts()
	accts[0].Client = upstream.NewWithChatEndpoint(5*time.Second, serverA.URL)
	accts[1].Client = upstream.NewWithChatEndpoint(5*time.Second, serverB.URL)
	h := NewHandler(Config{Pool: p, APIKey: testHandlerAPIKey, DefaultModel: "glm-5.2", MaxRotate: 3, ErrThreshold: 100})
	return h, p, &callsA, &callsB
}

func stickyPost(h *Handler, convID string) *httptest.ResponseRecorder {
	payload := `{"model":"glm-5.2","stream":false,"conversation_id":"` + convID + `","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authenticatedTestRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))
	return rec
}

func requireHealthy(t *testing.T, p *pool.Pool, name string) {
	t.Helper()
	if !p.Healthy(name) {
		t.Fatalf("账号 %s 应保持健康（未被冷却/禁用）", name)
	}
}

func stickyBinding(h *Handler, convID string) string {
	h.convMu.Lock()
	defer h.convMu.Unlock()
	return h.convAcct[convID].Account
}

// 黏性会话的上游瞬断（传输层 EOF，非 API 错误：不冷却账号、不退出轮转）后，
// 轮转必须换到其他健康账号，而不是把 MaxRotate 全部耗在同一个黏性账号上。
func TestStickyConversationFailsOverAfterTransientUpstreamError(t *testing.T) {
	h, p, callsA, callsB := stickyRotationHarness(t,
		func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) }, // 账号 a：始终瞬断
		nil, // 账号 b：默认正常 SSE
	)

	convID := strings.Repeat("ab", 16)                // 32 位 hex，构成显式会话
	h.storeConversation(convID, p.Accounts()[0].Name) // 黏性绑定账号 a（真实账号名）

	rec := stickyPost(h, convID)
	if rec.Code != http.StatusOK {
		t.Fatalf("黏性账号瞬断后应轮转到健康账号 b 并成功，got status=%d body=%s",
			rec.Code, truncateMsg(rec.Body.String(), 300))
	}
	if !strings.Contains(rec.Body.String(), `"content":"ok"`) {
		t.Fatalf("响应应由账号 b 的假上游产出，body=%s", truncateMsg(rec.Body.String(), 300))
	}
	if got := callsA.Load(); got != 1 {
		t.Fatalf("账号 a 的上游应只被尝试 1 次（失败即轮转），got=%d", got)
	}
	if got := callsB.Load(); got != 1 {
		t.Fatalf("账号 b 的上游应被命中 1 次，got=%d", got)
	}
	if bound := stickyBinding(h, convID); bound != p.Accounts()[1].Name {
		t.Fatalf("成功后黏性绑定应跟随实际服务的账号 b，got=%q", bound)
	}
	requireHealthy(t, p, t.Name()+"-a") // EOF 不冷却账号
	requireHealthy(t, p, t.Name()+"-b")
}

// 黏性账号正常时保持会话绑定，不产生多余的重试请求。
func TestStickyConversationKeepsBindingOnSuccess(t *testing.T) {
	h, p, callsA, callsB := stickyRotationHarness(t, nil, nil) // 两个上游都正常

	convID := strings.Repeat("cd", 16)
	h.storeConversation(convID, p.Accounts()[0].Name)

	rec := stickyPost(h, convID)
	if rec.Code != http.StatusOK {
		t.Fatalf("黏性请求应成功，got status=%d body=%s", rec.Code, truncateMsg(rec.Body.String(), 300))
	}
	if got := callsA.Load(); got != 1 {
		t.Fatalf("黏性账号上游应只被命中 1 次，got=%d", got)
	}
	if got := callsB.Load(); got != 0 {
		t.Fatalf("其他账号不应被触碰，got=%d", got)
	}
	if bound := stickyBinding(h, convID); bound != p.Accounts()[0].Name {
		t.Fatalf("成功后黏性绑定不应漂移，got=%q want=a", bound)
	}
	requireHealthy(t, p, t.Name()+"-a")
}

// 客户端在排队等待并发槽位期间断开：请求必须立即放弃（而不是空等 180s），
// 不触碰上游、不处罚账号，按 client_cancelled 收尾。
func TestQueueWaitAbortsOnClientDisconnect(t *testing.T) {
	h, p, callsA, callsB := stickyRotationHarness(t, nil, nil)
	acctA := p.Accounts()[0].Name
	acctB := p.Accounts()[1].Name
	if !p.AcquireLock(acctA) || !p.AcquireLock(acctB) {
		t.Fatal("占住池内全部账号的并发槽位")
	}
	defer p.ReleaseLock(acctA)
	defer p.ReleaseLock(acctB)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	req := authenticatedTestRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()

	start := time.Now()
	h.ServeHTTP(rec, req.WithContext(ctx))
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "client_cancelled") {
		t.Fatalf("客户端断开应按 client_cancelled 503 收尾，got status=%d body=%s",
			rec.Code, truncateMsg(rec.Body.String(), 300))
	}
	if elapsed > 15*time.Second {
		t.Fatalf("断开客户端后排队应立即放弃，实测耗时 %s（不应空等 180s）", elapsed)
	}
	if got := callsA.Load() + callsB.Load(); got != 0 {
		t.Fatalf("排队期间不应触碰上游，got=%d", got)
	}
	for _, name := range []string{acctA, acctB} {
		requireHealthy(t, p, name)
	}
}

// 账号目录已发现时，pickByCatalog 必须按并发利用率挑选空闲账号，避免全挤在 0 号账号排队。
func TestPickByCatalogLoadBalancesBusyAccount(t *testing.T) {
	h, p, callsA, callsB := stickyRotationHarness(t, nil, nil)
	acctA := p.Accounts()[0].Name
	if !p.AcquireLock(acctA) {
		t.Fatal("占住账号 a 的并发槽位")
	}
	defer p.ReleaseLock(acctA)

	req := authenticatedTestRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("空闲账号 b 应立即承接请求，got status=%d body=%s", rec.Code, truncateMsg(rec.Body.String(), 300))
	}
	if callsA.Load() != 0 || callsB.Load() != 1 {
		t.Fatalf("期望路由到空闲账号 b，got callsA=%d callsB=%d", callsA.Load(), callsB.Load())
	}
}
