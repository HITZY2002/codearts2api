package server

import (
	"encoding/json"
	"fmt"
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

func toolChoiceTestHandler(t *testing.T, endpoint string) (*Handler, *pool.Pool) {
	t.Helper()
	auths := []*auth.Auth{
		auth.New(t.Name()+"-a", "tester", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""),
		auth.New(t.Name()+"-b", "tester", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""),
	}
	p, err := pool.New(auths, pool.Config{MaxConcurrent: 1, ErrThreshold: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, acct := range p.Accounts() {
		acct.Client = upstream.NewWithChatEndpoint(time.Second, endpoint)
	}
	return NewHandler(Config{Pool: p, DefaultModel: "glm-5.2", MaxRotate: 2, ErrThreshold: 1}), p
}

func assertToolChoicePoolHealthy(t *testing.T, p *pool.Pool) {
	t.Helper()
	total, healthy, disabled, cooling, _ := p.Stats()
	if total != 2 || healthy != 2 || disabled != 0 || cooling != 0 {
		t.Fatalf("parameter error changed pool state: total=%d healthy=%d disabled=%d cooling=%d", total, healthy, disabled, cooling)
	}
	for _, state := range p.List() {
		if state["err_count"] != 0 || state["active_concurrent"] != 0 {
			t.Fatalf("parameter error penalized or retained an account lock: %#v", state)
		}
	}
}

func TestChatCompletionsPreservesNamedToolChoice(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			seen := make(chan map[string]any, 1)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer r.Body.Close()
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode upstream request: %v", err)
					return
				}
				seen <- body
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_weather\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer backend.Close()
			h, _ := toolChoiceTestHandler(t, backend.URL)
			payload := fmt.Sprintf(`{
				"model":"glm-5.2","stream":%t,
				"messages":[{"role":"user","content":"weather"}],
				"tools":[
					{"type":"function","function":{"name":"delete_file","parameters":{"type":"object"}}},
					{"type":"function","function":{"name":"get_weather","description":"weather only","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}
				],
				"tool_choice":{"type":"function","function":{"name":"get_weather"}}
			}`, stream)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"get_weather"`) || !strings.Contains(rec.Body.String(), `"finish_reason":"tool_calls"`) {
				t.Fatalf("named tool request failed: status=%d body=%s", rec.Code, rec.Body.String())
			}
			body := <-seen
			if body["tool_choice"] != "required" {
				t.Fatalf("upstream tool_choice=%#v, want string required", body["tool_choice"])
			}
			tools, _ := body["tools"].([]any)
			if len(tools) != 1 {
				t.Fatalf("upstream can choose an unintended tool: %#v", tools)
			}
			fn := tools[0].(map[string]any)["function"].(map[string]any)
			if fn["name"] != "get_weather" || fn["description"] != "weather only" || fn["parameters"] == nil {
				t.Fatalf("selected tool schema was changed: %#v", fn)
			}
		})
	}
}

func TestChatCompletionsRejectsInvalidNamedToolBeforeUpstream(t *testing.T) {
	for _, tools := range []string{
		`[]`,
		`[{"type":"function","function":{"name":"other"}}]`,
		`[{"type":"function","function":{"name":"target"}},{"type":"function","function":{"name":"target"}}]`,
	} {
		t.Run(tools, func(t *testing.T) {
			var attempts atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer backend.Close()
			h, p := toolChoiceTestHandler(t, backend.URL)
			payload := fmt.Sprintf(`{"messages":[{"role":"user","content":"hi"}],"tools":%s,"tool_choice":{"type":"function","function":{"name":"target"}}}`, tools)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))
			if rec.Code != http.StatusBadRequest || attempts.Load() != 0 {
				t.Fatalf("invalid named tool contacted upstream: status=%d attempts=%d body=%s", rec.Code, attempts.Load(), rec.Body.String())
			}
			assertToolChoicePoolHealthy(t, p)
		})
	}
}

func TestChatCompletionsParamInvalidDoesNotRotateOrCoolAccounts(t *testing.T) {
	for _, kind := range []string{"http", "first_sse", "late_sse"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", kind, stream), func(t *testing.T) {
				var attempts atomic.Int32
				var fail atomic.Bool
				fail.Store(true)
				backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					attempts.Add(1)
					if !fail.Load() {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
						return
					}
					if kind == "http" {
						w.WriteHeader(http.StatusBadRequest)
						_, _ = io.WriteString(w, `{"error":"InferHub.001001005.400 The request param is invalid"}`)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					if kind == "late_sse" {
						_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
					}
					_, _ = io.WriteString(w, "event: done\ndata: {\"error_code\":\"InferHub.001001005.400\",\"error_msg\":\"The request param is invalid\"}\n\n")
				}))
				defer backend.Close()
				h, p := toolChoiceTestHandler(t, backend.URL)
				payload := fmt.Sprintf(`{"messages":[{"role":"user","content":"hi"}],"stream":%t}`, stream)
				for i := 0; i < 3; i++ {
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))
					wantStatus := http.StatusBadRequest
					if kind == "late_sse" && stream {
						wantStatus = http.StatusOK // The stream headers were already sent.
						if !strings.Contains(rec.Body.String(), "event: error") || !strings.Contains(rec.Body.String(), "data: [DONE]") {
							t.Fatalf("late error did not terminate the stream: %s", rec.Body.String())
						}
					}
					if rec.Code != wantStatus || !strings.Contains(rec.Body.String(), "InferHub.001001005") {
						t.Fatalf("status=%d want=%d body=%s", rec.Code, wantStatus, rec.Body.String())
					}
					if attempts.Load() != int32(i+1) {
						t.Fatalf("parameter error rotated or retried accounts: attempts=%d requests=%d", attempts.Load(), i+1)
					}
					assertToolChoicePoolHealthy(t, p)
				}
				fail.Store(false)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"normal request"}]} `)))
				if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"content":"ok"`) {
					t.Fatalf("normal request failed after parameter errors: status=%d body=%s", rec.Code, rec.Body.String())
				}
				assertToolChoicePoolHealthy(t, p)
			})
		}
	}
}

func TestChatCompletionsServerErrorsStillCoolAccounts(t *testing.T) {
	var attempts atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"upstream unavailable"}`)
	}))
	defer backend.Close()
	h, p := toolChoiceTestHandler(t, backend.URL)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]} `)))
	_, healthy, disabled, cooling, _ := p.Stats()
	if rec.Code < 500 || attempts.Load() != 2 || healthy != 0 || disabled != 0 || cooling != 2 {
		t.Fatalf("server errors lost failover/cooldown behavior: status=%d attempts=%d healthy=%d disabled=%d cooling=%d", rec.Code, attempts.Load(), healthy, disabled, cooling)
	}
}
