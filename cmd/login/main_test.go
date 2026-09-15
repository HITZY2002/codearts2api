package main

import (
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// startCallback 起本地回调服务，返回 callback 地址。
func startCallback(t *testing.T) (string, *callbackState, chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	st := &callbackState{}
	codeCh := make(chan string, 1)
	go serveCallback(ln, "/oauth/callback", st, codeCh)
	return "http://" + ln.Addr().String() + "/oauth/callback", st, codeCh
}

// TestCallbackChain portal 会回调两次：先 secret+redirect（要求 307 跳回，
// 且轮询必须改用 portal 下发的 secret），再带 code。
func TestCallbackChain(t *testing.T) {
	base, st, codeCh := startCallback(t)
	client := &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	redirect := "https://codearts.huaweicloud.com/authorize?ticket_id=t1"
	resp, err := client.Get(base + "?secret=portal-secret&redirect=" + url.QueryEscape(redirect))
	if err != nil {
		t.Fatalf("first callback: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("first callback status=%d, want 307", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != redirect {
		t.Fatalf("first callback Location=%q, want %q", got, redirect)
	}
	if got := st.pollSecret("local-secret"); got != "portal-secret" {
		t.Fatalf("pollSecret=%q, want portal-secret", got)
	}

	resp2, err := client.Get(base + "?code=the-code&secret=portal-secret")
	if err != nil {
		t.Fatalf("second callback: %v", err)
	}
	body, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second callback status=%d body=%s", resp2.StatusCode, body)
	}
	if !strings.Contains(string(body), "登录成功") {
		t.Fatalf("second callback body=%s", body)
	}
	select {
	case code := <-codeCh:
		if code != "the-code" {
			t.Fatalf("code=%q, want the-code", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("code 未送到 channel")
	}
}

// TestCallbackNoCode 既无 code 也无 redirect：仍然报错，不能假装成功。
func TestCallbackNoCode(t *testing.T) {
	base, _, codeCh := startCallback(t)
	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Get(base)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", resp.StatusCode, body)
	}
	select {
	case code := <-codeCh:
		t.Fatalf("意外收到 code=%q", code)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestCallbackCodeInBody 兼容 POST body 投递 code（非表单 GET）。
func TestCallbackCodeInBody(t *testing.T) {
	base, _, codeCh := startCallback(t)
	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Post(base, "application/x-www-form-urlencoded", strings.NewReader("code=body-code"))
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	select {
	case code := <-codeCh:
		if code != "body-code" {
			t.Fatalf("code=%q, want body-code", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("code 未送到 channel")
	}
}
