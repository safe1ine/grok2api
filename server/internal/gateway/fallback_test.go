package gateway

import (
	"strings"
	"testing"
)

func TestFallbackTargetAvoidsDuplicateV1(t *testing.T) {
	if got := fallbackTarget("https://api.example.com/v1", "/v1/chat/completions"); got != "https://api.example.com/v1/chat/completions" {
		t.Fatalf("target = %q", got)
	}
	if got := fallbackTarget("https://api.example.com", "/v1/messages"); got != "https://api.example.com/v1/messages" {
		t.Fatalf("target = %q", got)
	}
}

func TestReplaceFallbackModelOnlyChangesModel(t *testing.T) {
	body := []byte(`{"model":"grok-4.6","tools":[{"input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`)
	got := replaceFallbackModel(body, "gpt-4.1")
	if string(got) == string(body) {
		t.Fatal("model was not replaced")
	}
	if string(got) == "" || !containsAll(string(got), `"model":"gpt-4.1"`, `"input_schema":{"type":"object"}`) {
		t.Fatalf("fallback body = %s", got)
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}
