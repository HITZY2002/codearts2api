package pool

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/upstream"
)

func TestRefreshTokenUsesAccountClientID(t *testing.T) {
	var seenClientID atomic.Value
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if id := r.FormValue("client_id"); id != "" {
			seenClientID.Store(id)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"refresh_token":"new-rt","credentials":{"access_key_id":"new-ak","secret_access_key":"new-sk","security_token":"new-st","expiration":"2099-01-01T00:00:00Z"}}`)
	}))
	defer sts.Close()

	dpopKey, err := upstream.NewDPoPPrivateJWK()
	if err != nil {
		t.Fatal(err)
	}
	a := auth.New("uid-custom-client", "tester", "dom", "old-st", "old-ak", "old-sk",
		time.Now().Add(time.Minute).Format(time.RFC3339), "old-rt", "verifier", dpopKey)
	a.SetClientID("custom-ide-client")
	a.SetPath(filepath.Join(t.TempDir(), "codearts-uid-custom-client.json"))

	lc := upstream.DefaultLoginConfig()
	lc.STSHost = sts.URL
	lc.ClientID = "fallback-client"

	p, err := New([]*auth.Auth{a}, Config{LoginConfig: lc}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.RefreshToken("uid-custom-client"); err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	got, _ := seenClientID.Load().(string)
	if got != "custom-ide-client" {
		t.Fatalf("RefreshToken sent client_id=%q, want %q", got, "custom-ide-client")
	}
}
