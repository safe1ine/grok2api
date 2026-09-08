package api

import (
	"strings"
	"testing"

	"grok2api/server/internal/auth"
)

func TestNewAPIKey(t *testing.T) {
	plain, hash, prefix, err := newAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, "sk-grok2api-") || len(plain) != 60 {
		t.Fatalf("unexpected key format: length=%d", len(plain))
	}
	if prefix != plain[:16] || hash != auth.HashKey(plain) {
		t.Fatal("key metadata does not match plaintext")
	}
}
