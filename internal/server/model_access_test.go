package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

const (
	modelAccessErrorJSON = `{"error_code":"TM.000010051","error_msg":"您当前无权调用该模型，请联系企业管理员开通。"}`
	modelAccessTestReply = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
)

func TestModelIDFallback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream bool
		probe  bool
	}{
		{name: "HTTP 502"},
		{name: "SSE error", stream: true},
		{name: "SSE role then error", stream: true},
		{name: "background probe", probe: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const canonical, alternate = "OpenPangu-2.0-Pro", "openpangu-2.0-pro"
			seen := make(chan string, 8)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				model := readUpstreamModel(t, r)
				seen <- model
				if model == alternate {
					_, _ = io.WriteString(w, modelAccessTestReply)
					return
				}
				switch tc.name {
				case "SSE role then error":
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
					fallthrough
				case "SSE error":
					_, _ = fmt.Fprintf(w, "event: done\ndata: %s\n\n", modelAccessErrorJSON)
				default:
					w.WriteHeader(http.StatusBadGateway)
					_, _ = io.WriteString(w, modelAccessErrorJSON)
				}
			}))
			defer backend.Close()
			h, p := modelAccessTestHandler(t, backend.URL, "/a")
			if tc.probe {
				h.probeModel(p.Accounts()[0], canonical)
				if st, _ := modelAvailability(p.Accounts()[0].UID, canonical); st != availUsable {
					t.Fatalf("probe recorded availability %v, want usable", st)
				}
			} else {
				rec := modelAccessTestChat(h, alternate, tc.stream)
				if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"content":"ok"`) || strings.Contains(rec.Body.String(), "TM.000010051") {
					t.Fatalf("first chat returned %d: %s", rec.Code, rec.Body)
				}
			}
			rec := modelAccessTestChat(h, canonical, tc.stream)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"content":"ok"`) {
				t.Fatalf("second chat returned %d: %s", rec.Code, rec.Body)
			}
			assertModelAttempts(t, seen, canonical, alternate, alternate)
		})
	}
}

func TestModelAccessDeniedKeepsAccountHealthy(t *testing.T) {
	const canonical, alternate = "OpenPangu-2.0-Pro", "openpangu-2.0-pro"
	seen := make(chan string, 16)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := readUpstreamModel(t, r)
		seen <- r.URL.Path + ":" + model
		if r.URL.Path == "/a" && strings.EqualFold(model, canonical) {
			if model == alternate {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"error_code":"InferHub.002002009.404","error_msg":"model is not registered"}`)
				return
			}
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, modelAccessErrorJSON)
			return
		}
		_, _ = io.WriteString(w, modelAccessTestReply)
	}))
	defer backend.Close()
	h, p := modelAccessTestHandler(t, backend.URL, "/a", "/b")
	for _, model := range []string{canonical, canonical, "GLM-5.2"} {
		if rec := modelAccessTestChat(h, model, false); rec.Code != http.StatusOK {
			t.Fatalf("%s returned %d: %s", model, rec.Code, rec.Body)
		}
	}
	assertModelAttempts(t, seen, "/a:"+canonical, "/a:"+alternate, "/b:"+canonical, "/b:"+canonical, "/a:GLM-5.2")
	for _, state := range p.List() {
		if state["err_count"] != 0 || state["active_concurrent"] != 0 {
			t.Fatalf("model denial penalized the account or retained its lock: %#v", state)
		}
	}
	if !p.Healthy(p.Accounts()[0].Name) {
		t.Fatal("model access error cooled the account")
	}
	if st, reason := modelAvailability(p.Accounts()[0].UID, canonical); st != availUnusable || reason != modelAccessDeniedReason {
		t.Fatalf("availability=%v, reason=%q", st, reason)
	}
}

func TestModelAccessDeniedAfterContent(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			const model = "OpenPangu-2.0-Pro"
			seen := make(chan string, 8)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- readUpstreamModel(t, r)
				_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\nevent: done\ndata: %s\n\n", modelAccessErrorJSON)
			}))
			defer backend.Close()
			h, p := modelAccessTestHandler(t, backend.URL, "/a")
			rec := modelAccessTestChat(h, model, stream)
			if stream {
				if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "event: error") {
					t.Fatalf("stream returned %d: %s", rec.Code, rec.Body)
				}
			} else if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "model_not_available") {
				t.Fatalf("chat returned %d: %s", rec.Code, rec.Body)
			}
			assertModelAttempts(t, seen, model)
			if !p.Healthy(p.Accounts()[0].Name) {
				t.Fatal("late model error cooled the account")
			}
		})
	}
}

func modelAccessTestHandler(t *testing.T, endpoint string, paths ...string) (*Handler, *pool.Pool) {
	t.Helper()
	var auths []*auth.Auth
	accountPrefix := t.Name() + "-" + randHex(16)
	for _, path := range paths {
		auths = append(auths, auth.New(accountPrefix+path, "tester", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""))
	}
	p, err := pool.New(auths, pool.Config{ErrThreshold: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	for i, acct := range p.Accounts() {
		acct.Client = upstream.NewWithChatEndpoint(time.Second, endpoint+paths[i])
	}
	return NewHandler(Config{Pool: p, ErrThreshold: 1, MaxRotate: len(paths)}), p
}

func modelAccessTestChat(h *Handler, model string, stream bool) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":%q}]}`, model, stream, "hi "+model)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	return rec
}

func readUpstreamModel(t *testing.T, r *http.Request) string {
	t.Helper()
	var body struct{ Model string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("decode upstream body: %v", err)
	}
	return body.Model
}

func assertModelAttempts(t *testing.T, attempts chan string, want ...string) {
	t.Helper()
	var got []string
	for len(attempts) > 0 {
		got = append(got, <-attempts)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("upstream model attempts = %v, want %v", got, want)
	}
}
