package server

import (
	"bufio"
	"encoding/json"
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

func TestChatCompletionsStreamsNativeToolCallsWithoutBuffering(t *testing.T) {
	requestSeen := make(chan map[string]any, 1)
	headersSeen := make(chan http.Header, 1)
	releaseUpstream := make(chan struct{})

	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream request: %v", err)
			return
		}
		requestSeen <- body
		headersSeen <- r.Header.Clone()

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"upstream-1\",\"object\":\"chat.completion.chunk\",\"model\":\"GLM-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"checking\"}}]}\n\n")
		w.(http.Flusher).Flush()

		<-releaseUpstream
		_, _ = io.WriteString(w, "data: {\"id\":\"upstream-1\",\"object\":\"chat.completion.chunk\",\"model\":\"GLM-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_native\",\"type\":\"function\",\"function\":{\"name\":\"bash\",\"arguments\":\"\"}}]}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"upstream-1\",\"object\":\"chat.completion.chunk\",\"model\":\"GLM-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"command\\\":\\\"pwd\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstreamServer.Close()

	a := auth.New("test-account", "tester", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{a}, pool.Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.Accounts()[0].Client = upstream.NewWithChatEndpoint(5*time.Second, upstreamServer.URL)

	apiServer := httptest.NewServer(NewHandler(Config{Pool: p, DefaultModel: "glm-5.2", MaxRotate: 1}))
	defer apiServer.Close()

	payload := `{
		"model":"glm-5.2",
		"stream":true,
		"messages":[
			{"role":"system","content":"use tools"},
			{"role":"user","content":"inspect"},
			{"role":"assistant","content":null,"reasoning_content":"need pwd","tool_calls":[{"id":"call_old","type":"function","function":{"name":"bash","arguments":"{\"command\":\"pwd\"}"}}]},
			{"role":"tool","tool_call_id":"call_old","content":"/workspace"}
		],
		"tools":[{"type":"function","function":{"name":"bash","description":"run shell","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}}],
		"tool_choice":"auto"
	}`
	req, err := http.NewRequest(http.MethodPost, apiServer.URL+"/v1/chat/completions", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")

	responseCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			errCh <- err
			return
		}
		responseCh <- resp
	}()

	var resp *http.Response
	select {
	case err := <-errCh:
		close(releaseUpstream)
		t.Fatal(err)
	case resp = <-responseCh:
		// Native streaming must expose the first reasoning delta while the
		// upstream request is still running.
	case <-time.After(500 * time.Millisecond):
		close(releaseUpstream)
		select {
		case resp = <-responseCh:
			_ = resp.Body.Close()
		case <-time.After(2 * time.Second):
		}
		t.Fatal("downstream response was buffered until the upstream tool call completed")
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	firstEvent, err := reader.ReadString('\n')
	if err != nil {
		close(releaseUpstream)
		t.Fatal(err)
	}
	if !strings.Contains(firstEvent, `"reasoning_content":"checking"`) {
		close(releaseUpstream)
		t.Fatalf("first streamed event=%q", firstEvent)
	}

	close(releaseUpstream)
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	stream := firstEvent + string(rest)
	if !strings.Contains(stream, `"tool_calls"`) || !strings.Contains(stream, `"finish_reason":"tool_calls"`) {
		t.Fatalf("native tool stream was not preserved: %s", stream)
	}

	upstreamBody := <-requestSeen
	messages, _ := upstreamBody["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("upstream messages were flattened: %#v", upstreamBody["messages"])
	}
	assistant, _ := messages[2].(map[string]any)
	if assistant["role"] != "assistant" || assistant["reasoning_content"] != "need pwd" || assistant["tool_calls"] == nil {
		t.Fatalf("assistant history was not preserved: %#v", assistant)
	}
	toolResult, _ := messages[3].(map[string]any)
	if toolResult["role"] != "tool" || toolResult["tool_call_id"] != "call_old" {
		t.Fatalf("tool result was not preserved: %#v", toolResult)
	}
	if tools, _ := upstreamBody["tools"].([]any); len(tools) != 1 {
		t.Fatalf("native tools missing from upstream body: %#v", upstreamBody["tools"])
	}

	headers := <-headersSeen
	if headers.Get("Chat-Id") == "" || headers.Get("Session-Id") == "" {
		t.Fatalf("CodeArts affinity headers missing: %#v", headers)
	}
}

func TestChatCompletionsRetriesEmbeddedCodeArtsQueueLimit(t *testing.T) {
	var attempts atomic.Int32
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if attempts.Add(1) == 1 {
			_, _ = io.WriteString(w, "data: {\"text\":\"[DONE]\",\"error_code\":\"InferHub.ModelArts.81111.429\",\"error_msg\":\"TPM limit reached\"}\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"recovered\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstreamServer.Close()

	a := auth.New("queue-account", "tester", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{a}, pool.Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.Accounts()[0].Client = upstream.NewWithChatEndpoint(5*time.Second, upstreamServer.URL)
	h := NewHandler(Config{
		Pool: p, DefaultModel: "glm-5.2", MaxRotate: 1,
		QueueRetryDelay: time.Millisecond, QueueMaxAttempts: 2,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"glm-5.2","stream":false,"messages":[{"role":"user","content":"hi"}]
	}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if attempts.Load() != 2 || !strings.Contains(rec.Body.String(), `"content":"recovered"`) {
		t.Fatalf("queue retry did not recover: attempts=%d body=%s", attempts.Load(), rec.Body.String())
	}
}

func TestChatCompletionsSignsBenefitHeaderForGLM53Flash(t *testing.T) {
	headersSeen := make(chan http.Header, 1)
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headersSeen <- r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstreamServer.Close()

	a := auth.New("benefit-account", "tester", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{a}, pool.Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.Accounts()[0].Client = upstream.NewWithChatEndpoint(5*time.Second, upstreamServer.URL)
	h := NewHandler(Config{Pool: p, DefaultModel: "glm-5.3-flash", MaxRotate: 1})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"glm-5.3-flash","stream":false,"messages":[{"role":"user","content":"hi"}]
	}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	headers := <-headersSeen
	if got := headers.Get("maas_type"); got != "benefit" {
		t.Fatalf("maas_type=%q, want benefit", got)
	}
	authorization := headers.Get("Authorization")
	if !strings.Contains(authorization, "SignedHeaders=") || !strings.Contains(authorization, "maas_type") {
		t.Fatalf("maas_type was not covered by the Huawei signature: %q", authorization)
	}
}

func TestModelsAdvertisesOnlyVerifiedNewCodeArtsModels(t *testing.T) {
	p, err := pool.New(nil, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int    `json:"context_length"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	// 静态兜底只列实测确认可用的模型；未注册的（GLM-5.2-ArkTS-SPARK /
	// OpenPangu-2.0-Pro/Flash）不得出现，否则等于把 404 写进兜底路径。
	for _, bad := range []string{"GLM-5.2-ArkTS-SPARK", "OpenPangu-2.0-Pro", "OpenPangu-2.0-Flash"} {
		for _, model := range payload.Data {
			if model.ID == bad {
				t.Fatalf("unregistered model %s must not be advertised; body=%s", bad, rec.Body.String())
			}
		}
	}
	want := map[string]int{"deepseek-v4-pro-0813": 1048576, "GLM-5.2": 202752}
	for _, model := range payload.Data {
		if expected, ok := want[model.ID]; ok {
			if model.ContextLength != expected {
				t.Fatalf("model %s context_length=%d, want %d", model.ID, model.ContextLength, expected)
			}
			delete(want, model.ID)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing models from /v1/models: %#v; body=%s", want, rec.Body.String())
	}
}
