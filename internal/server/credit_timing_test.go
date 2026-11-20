package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

// TestCreditUsageQueriedOnlyAtTurnEnd 验证积分查询时机对齐官方桌面客户端：
// 官方 renderer 在 session 终态（recordTurnStats）时按 user-msg-id 查一次；
// 网关侧 finish_reason=tool_calls 的续请求不查，最终 stop 的请求查整轮汇总。
func TestCreditUsageQueriedOnlyAtTurnEnd(t *testing.T) {
	var mu sync.Mutex
	var chatMsgIDs []string
	var creditIDs []string
	turn := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case upstream.EpChatV2:
			mu.Lock()
			chatMsgIDs = append(chatMsgIDs, r.Header.Get("user-msg-id"))
			n := turn
			turn++
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			if n == 0 {
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"bash\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			} else {
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}
		case upstream.EpCreditUsage:
			mu.Lock()
			creditIDs = append(creditIDs, r.URL.Query().Get("id"))
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"data":{"creditCost":0.5,"promptTokenUsed":10,"completionTokenUsed":2,"totalTokenUsed":12,"requestCount":2}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	a := auth.New(t.Name()+"-acct", "tester", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{a}, pool.Config{ErrThreshold: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	useFreshTestCatalogs(p)
	p.Accounts()[0].Client = upstream.NewWithSnapEndpoint(2*time.Second, backend.URL)
	h := NewHandler(Config{Pool: p, APIKey: testHandlerAPIKey, ErrThreshold: 1})

	tools := `"tools":[{"type":"function","function":{"name":"bash","description":"run","parameters":{"type":"object","properties":{}}}}]`
	first := fmt.Sprintf(`{"model":"GLM-5.2","stream":false,%s,"messages":[{"role":"user","content":"list files"}]}`, tools)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authenticatedTestRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(first)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"finish_reason":"tool_calls"`) {
		t.Fatalf("tool turn status=%d body=%s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	if len(creditIDs) != 0 {
		mu.Unlock()
		t.Fatalf("credit queried before turn end: %v", creditIDs)
	}
	mu.Unlock()

	second := fmt.Sprintf(`{"model":"GLM-5.2","stream":false,%s,"messages":[{"role":"user","content":"list files"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"a.txt"}]}`, tools)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authenticatedTestRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(second)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"finish_reason":"stop"`) {
		t.Fatalf("final turn status=%d body=%s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(creditIDs) != 1 {
		t.Fatalf("credit queries=%v, want exactly one at turn end", creditIDs)
	}
	if len(chatMsgIDs) != 2 || chatMsgIDs[0] == "" || chatMsgIDs[0] != chatMsgIDs[1] {
		t.Fatalf("turn did not reuse one msg id: %v", chatMsgIDs)
	}
	if creditIDs[0] != chatMsgIDs[0] {
		t.Fatalf("credit query id=%q, want turn msg id %q", creditIDs[0], chatMsgIDs[0])
	}
}
