package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDirSkipsMalformedCredential(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codearts-good.json"), []byte(`{"user_id":"good","cloud_dragon_token":"token"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "codearts-bad.json"), []byte(`{"broken"`), 0o600); err != nil {
		t.Fatal(err)
	}
	accounts, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("malformed credential blocked valid accounts: %v", err)
	}
	if len(accounts) != 1 || accounts[0].UserID != "good" {
		t.Fatalf("loaded accounts=%v", accounts)
	}
}

func TestInvalidExpirationRequiresRefresh(t *testing.T) {
	account := &Auth{Expiration: "not-a-time"}
	if remaining := account.Remaining(); remaining != 0 {
		t.Fatalf("invalid expiration remaining=%s, want 0", remaining)
	}
	if !account.Expired() {
		t.Fatal("invalid expiration must not look valid for 24 hours")
	}
}
