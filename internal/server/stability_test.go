package server

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

	"codearts2api/internal/upstream"
)

func TestConversationStateConcurrentSaveAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversations.json")
	h := NewHandler(Config{ConvStateFile: path})
	const count = 32
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.storeConversation(fmt.Sprintf("%032x", i+1), fmt.Sprintf("account-%d", i))
		}()
	}
	wg.Wait()

	// 落盘是延迟合并的：显式 flush 模拟进程退出前的 FlushConversationState。
	h.FlushConversationState()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state conversationState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("conversation state is not valid JSON: %v", err)
	}
	if len(state.Conversations) != count {
		t.Fatalf("saved conversations=%d, want %d", len(state.Conversations), count)
	}

	reloaded := NewHandler(Config{ConvStateFile: path})
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("%032x", i+1)
		if got := reloaded.convAcct[id].Account; got != fmt.Sprintf("account-%d", i) {
			t.Fatalf("reloaded conversation %s account=%q", id, got)
		}
	}
}

func TestConversationStatePersistsWithoutExplicitFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversations.json")
	h := NewHandler(Config{ConvStateFile: path})
	id := fmt.Sprintf("%032x", 42)
	h.storeConversation(id, "account-async")
	deadline := time.Now().Add(3 * time.Second)
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			var state conversationState
			if json.Unmarshal(raw, &state) == nil && state.Conversations[id].Account == "account-async" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("conversation state was not persisted by the background flush")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWithAuthRejectsEmptyAPIKey(t *testing.T) {
	h := NewHandler(Config{})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("empty API key status=%d, want 503", rec.Code)
	}
}

func TestQueueReacquireFailureDoesNotReleaseAnotherRequest(t *testing.T) {
	firstRequest := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-firstRequest:
		default:
			close(firstRequest)
		}
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer backend.Close()
	h, p := modelAccessTestHandler(t, backend.URL, "/a")
	h.cfg.QueueRetryDelay = 20 * time.Millisecond
	h.cfg.QueueReacquireTimeout = 5 * time.Millisecond
	h.cfg.QueueMaxAttempts = 2
	accountName := p.Accounts()[0].Name

	holderAcquired := make(chan struct{})
	releaseHolder := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseHolder) }) }
	defer release()
	var holderWG sync.WaitGroup
	holderWG.Add(1)
	go func() {
		defer holderWG.Done()
		<-firstRequest
		if p.AcquireLockWait(accountName, time.Second) {
			close(holderAcquired)
			<-releaseHolder
			p.ReleaseLock(accountName)
		}
	}()

	_ = modelAccessTestChat(h, "GLM-5.2", false)
	select {
	case <-holderAcquired:
	case <-time.After(time.Second):
		t.Fatal("competing request did not acquire the account slot")
	}
	if got := p.List()[0]["active_concurrent"]; got != 1 {
		t.Fatalf("competing holder slot was released: active_concurrent=%v", got)
	}
	release()
	holderWG.Wait()
	if got := p.List()[0]["active_concurrent"]; got != 0 {
		t.Fatalf("slot not released by its owner: active_concurrent=%v", got)
	}
}

func TestChatCompletionsUsesModelCapabilityRouting(t *testing.T) {
	const model = "capability-route-test-model"
	seen := make(chan string, 4)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.URL.Path + ":" + readUpstreamModel(t, r)
		_, _ = w.Write([]byte(modelAccessTestReply))
	}))
	defer backend.Close()
	h, p := modelAccessTestHandler(t, backend.URL, "/a", "/b", "/c", "/d")
	for i, acct := range p.Accounts() {
		modelID := "GLM-5.2"
		if i == 3 {
			modelID = model
		}
		upstream.SetAccountModels(acct.UID, []upstream.ModelInfo{{ID: modelID}})
	}

	rec := modelAccessTestChat(h, model, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("capability routed chat status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertModelAttempts(t, seen, "/d:"+model)
}

func TestLateSSEErrorsUseConsistentAccountClassification(t *testing.T) {
	tests := []struct {
		name, code, message string
		wantDisabled        bool
	}{
		{name: "unauthorized", code: "IAM.401", message: "unauthorized", wantDisabled: true},
		{name: "rate limited", code: "quota.429", message: "rate limited"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\nevent: done\ndata: {\"error_code\":%q,\"error_msg\":%q}\n\n", tc.code, tc.message)
			}))
			defer backend.Close()
			h, p := modelAccessTestHandler(t, backend.URL, "/a")
			_ = modelAccessTestChat(h, "GLM-5.2", false)
			state := p.List()[0]
			if disabled, _ := state["disabled"].(bool); disabled != tc.wantDisabled {
				t.Fatalf("disabled=%t, want %t; state=%#v", disabled, tc.wantDisabled, state)
			}
			cooling, _ := state["cooling"].(bool)
			if cooling == tc.wantDisabled {
				t.Fatalf("cooling=%t disabled=%t, want exactly one account classification", cooling, tc.wantDisabled)
			}
		})
	}
}
