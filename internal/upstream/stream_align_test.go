package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 官方对齐回归：请求体带 stream_options.include_usage（官方 chat_core.rs 同款），
// 请求头带 x-ot-trace-id / x-ot-span-id / created-time（官方 billing.rs 头族中
// 语义确定的子集），上游真实 usage 优先于估算。
func TestChatV2BodyAndHeadersOfficialAligned(t *testing.T) {
	var gotBody map[string]any
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := NewWithChatEndpoint(5*time.Second, srv.URL)
	rc, err := c.ChatStreamWithOptions(context.Background(),
		[]ChatMessage{{Role: "user", Content: "hi"}}, "",
		SignCredential{AccessKeyID: "ak", SecretAccessKey: "sk", SecurityToken: "token"},
		"user", "GLM-5.2", ChatOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	so, ok := gotBody["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Fatalf("stream_options.include_usage 应为 true, got %v", gotBody["stream_options"])
	}
	for _, h := range []string{"x-ot-trace-id", "x-ot-span-id", "created-time"} {
		if gotHeaders.Get(h) == "" {
			t.Fatalf("缺少官方头 %s", h)
		}
	}
	if n := len(gotHeaders.Get("x-ot-trace-id")); n != 32 {
		t.Fatalf("x-ot-trace-id 应为 32 hex, got len=%d", n)
	}

	// 上游真实 usage 应被捕获（usageEstimate 优先采用）
	comp, err := AggregateRaw(rc)
	if err != nil {
		t.Fatal(err)
	}
	if comp.Usage == nil {
		t.Fatal("应捕获上游 usage")
	}
	if pt, _ := comp.Usage["prompt_tokens"].(float64); pt != 11 {
		t.Fatalf("prompt_tokens=%v, want 11", comp.Usage["prompt_tokens"])
	}
}

// OpenAI 规范：流式首个 delta 带 role（引导 chunk 注入，上游自带 role 不重复）。
func TestStreamLeadingRoleChunk(t *testing.T) {
	rec := httptest.NewRecorder()
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: [DONE]\n\n"
	if err := StreamCapture(rec, bytes.NewBufferString(stream), "m", nil); err != nil {
		t.Fatal(err)
	}
	first := ""
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data: ") && trimmed != "data: [DONE]" {
			first = strings.TrimPrefix(trimmed, "data: ")
			break
		}
	}
	var chunk struct {
		Choices []struct {
			Delta map[string]any `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(first), &chunk); err != nil {
		t.Fatalf("首个 chunk 解析失败: %v (%s)", err, first)
	}
	if len(chunk.Choices) == 0 {
		t.Fatalf("首 chunk 缺 choices: %s", first)
	}
	if chunk.Choices[0].Delta["role"] != "assistant" {
		t.Fatalf("首 chunk delta 应含 role=assistant: %s", first)
	}
	// role 只出现一次：后续内容 chunk 不再带 role
	roleCount := strings.Count(rec.Body.String(), `"role":"assistant"`)
	if roleCount != 1 {
		t.Fatalf("role 应恰好出现 1 次, got %d, body=%s", roleCount, rec.Body.String())
	}
}

// event: error 携带"结构化但无可识别错误码"的载荷（如 {"message":"boom"}）：
// 不能被吞成空内容成功（独立验证发现的缺口）。error_code=0 仍视为正常帧。
func TestErrorEventStructuredWithoutCode(t *testing.T) {
	stream := "event: error\ndata: {\"message\":\"boom\"}\n\n"
	_, err := AggregateRaw(bytes.NewBufferString(stream))
	var ue *UpstreamError
	if err == nil || !errors.As(err, &ue) || ue.Msg != "boom" {
		t.Fatalf("无错误码的结构化 error 事件应报错, got err=%v", err)
	}
}

// error 为字符串（非标准信封）：整串即错误消息。
func TestChunkErrorStringForm(t *testing.T) {
	stream := "data: {\"error\":\"boom\"}\n\n"
	_, err := AggregateRaw(bytes.NewBufferString(stream))
	var ue *UpstreamError
	if err == nil || !errors.As(err, &ue) || ue.Msg != "boom" {
		t.Fatalf("err=%v", err)
	}
}
