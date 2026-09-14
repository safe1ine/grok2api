package gateway

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func TestStreamCopyRawWithMetricsJSONIsUnchanged(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":12,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":4}},"choices":[]}`)
	writer := httptest.NewRecorder()
	metrics := streamCopyRawWithMetrics(writer, bytes.NewReader(body), "application/json", time.Now())
	if !bytes.Equal(writer.Body.Bytes(), body) {
		t.Fatalf("body changed: %s", writer.Body.Bytes())
	}
	if metrics.InputTokens != 12 || metrics.CachedInputTokens != 4 || metrics.OutputTokens != 3 {
		t.Fatalf("metrics = %+v", metrics)
	}
}

func TestStreamCopyRawWithMetricsSSEIsUnchanged(t *testing.T) {
	body := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9,\"cache_read_input_tokens\":2}}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	writer := httptest.NewRecorder()
	metrics := streamCopyRawWithMetrics(writer, bytes.NewReader(body), "text/event-stream", time.Now())
	if !bytes.Equal(writer.Body.Bytes(), body) {
		t.Fatalf("body changed: %q", writer.Body.Bytes())
	}
	if metrics.InputTokens != 9 || metrics.CachedInputTokens != 2 || metrics.OutputTokens != 5 || !metrics.StreamCompleted {
		t.Fatalf("metrics = %+v", metrics)
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
