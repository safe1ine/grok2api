package gateway

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestUnsupportedArgumentFallback(t *testing.T) {
	t.Parallel()

	errorBody := []byte(`{"code":"400","error":"Argument not supported: external_web_access"}`)
	argument := unsupportedArgument(errorBody)
	if argument != "external_web_access" {
		t.Fatalf("argument = %q", argument)
	}
	body := []byte(`{"model":"grok-4.6","input":"hello","external_web_access":true,"tools":[{"type":"web_search","external_web_access":false},{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"external_web_access":{"type":"boolean"}}}}]}`)
	normalized, changed := removeUnsupportedArgument(body, argument)
	if !changed {
		t.Fatal("expected unsupported argument to be removed")
	}
	payload := decodeStatePayload(t, normalized)
	if _, exists := payload["external_web_access"]; exists {
		t.Fatal("top-level unsupported argument retained")
	}
	tools := payload["tools"].([]any)
	if payload["input"] != "hello" || len(tools) != 2 {
		t.Fatalf("ordinary request fields changed: %s", normalized)
	}
	webSearch := tools[0].(map[string]any)
	if webSearch["type"] != "web_search" {
		t.Fatalf("web_search tool removed: %s", normalized)
	}
	if _, exists := webSearch["external_web_access"]; exists {
		t.Fatal("nested web_search argument retained")
	}
	properties := tools[1].(map[string]any)["parameters"].(map[string]any)["properties"].(map[string]any)
	if _, exists := properties["external_web_access"]; !exists {
		t.Fatal("function parameter schema was modified")
	}
}

func TestUnsupportedArgumentFallbackRejectsUnsafeOrMalformedNames(t *testing.T) {
	t.Parallel()

	for _, body := range []string{
		`{"error":"Argument not supported: input.foo"}`,
		`{"error":"Argument not supported: external web access"}`,
		`{"error":"other"}`,
		`not-json`,
	} {
		if got := unsupportedArgument([]byte(body)); got != "" {
			t.Errorf("unsupportedArgument(%q) = %q", body, got)
		}
	}
	request := []byte(`{"model":"grok-4.6","input":"hello"}`)
	for _, protected := range []string{"model", "input", "messages", "tools", "missing"} {
		if normalized, changed := removeUnsupportedArgument(request, protected); changed || !bytes.Equal(normalized, request) {
			t.Errorf("protected argument %q changed request: %s", protected, normalized)
		}
	}
}

func TestClassifyStateCompatibilityError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		body string
		want stateCompatibilityIssue
	}{
		{`{"code":"invalid-argument","error":"Could not decrypt the provided encrypted_content. Ensure the value is the unmodified encrypted_content from a previous response."}`, issueEncryptedContent},
		{`{"code":"invalid-argument","error":"Invalid reasoning effort."}`, issueReasoningEffort},
		{`{"code":"invalid-argument","error":"Could not decode the compaction blob. Ensure it is unmodified from the compact response."}`, issueCompactionBlob},
		{`{"error":"other"}`, ""},
	}
	for _, test := range tests {
		if got := classifyStateCompatibilityError([]byte(test.body)); got != test.want {
			t.Errorf("classify %s = %q, want %q", test.body, got, test.want)
		}
	}
}

func TestApplyStateCompatibilityFallbackRemovesOpaqueState(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model":"grok-4.6",
		"previous_response_id":"resp_previous",
		"input":[
			{"type":"reasoning","id":"rs_1","encrypted_content":"encrypted-secret","summary":[]},
			{"type":"compaction","id":"cmp_1","encrypted_content":"compaction-secret"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"keep this prompt"}]},
			{"type":"function_call_output","call_id":"call_1","output":"keep this tool result"}
		]
	}`)

	for _, issue := range []stateCompatibilityIssue{issueEncryptedContent, issueCompactionBlob} {
		normalized, changed := applyStateCompatibilityFallback(body, issue)
		if !changed {
			t.Fatalf("issue %q did not change request", issue)
		}
		payload := decodeStatePayload(t, normalized)
		if _, exists := payload["previous_response_id"]; exists {
			t.Fatalf("issue %q retained previous_response_id", issue)
		}
		input := payload["input"].([]any)
		if len(input) != 2 {
			t.Fatalf("issue %q input length = %d, want 2: %s", issue, len(input), normalized)
		}
		if input[0].(map[string]any)["type"] != "message" || input[1].(map[string]any)["type"] != "function_call_output" {
			t.Fatalf("issue %q removed ordinary history: %s", issue, normalized)
		}
		if !bytes.Contains(normalized, []byte("keep this prompt")) || !bytes.Contains(normalized, []byte("keep this tool result")) {
			t.Fatalf("issue %q lost ordinary content: %s", issue, normalized)
		}
		if bytes.Contains(normalized, []byte("encrypted-secret")) || bytes.Contains(normalized, []byte("compaction-secret")) {
			t.Fatalf("issue %q retained opaque state", issue)
		}
	}
}

func TestApplyStateCompatibilityFallbackRemovesOnlyRequestReasoningEffort(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model":"grok-4.6",
		"reasoning_effort":"none",
		"reasoning":{"effort":"minimal","summary":"auto"},
		"tools":[{"type":"function","name":"spawn","parameters":{"type":"object","properties":{"reasoning_effort":{"type":"string"}}}}],
		"input":"hello"
	}`)
	normalized, changed := applyStateCompatibilityFallback(body, issueReasoningEffort)
	if !changed {
		t.Fatal("expected reasoning effort fallback")
	}
	payload := decodeStatePayload(t, normalized)
	if _, exists := payload["reasoning_effort"]; exists {
		t.Fatal("top-level reasoning_effort retained")
	}
	reasoning := payload["reasoning"].(map[string]any)
	if _, exists := reasoning["effort"]; exists {
		t.Fatal("nested reasoning effort retained")
	}
	if reasoning["summary"] != "auto" {
		t.Fatalf("reasoning summary changed: %#v", reasoning)
	}
	properties := payload["tools"].([]any)[0].(map[string]any)["parameters"].(map[string]any)["properties"].(map[string]any)
	if _, exists := properties["reasoning_effort"]; !exists {
		t.Fatal("tool schema property reasoning_effort was removed")
	}
}

func TestStateCompatibilitySummaryDoesNotLeakContent(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"reasoning":{"effort":"none"},
		"previous_response_id":"secret-response-id",
		"input":[
			{"type":"reasoning","encrypted_content":"secret-encrypted-value"},
			{"type":"message","role":"user","content":"secret-prompt"}
		]
	}`)
	raw, err := json.Marshal(stateCompatibilitySummary(body))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range [][]byte{[]byte("secret-response-id"), []byte("secret-encrypted-value"), []byte("secret-prompt")} {
		if bytes.Contains(raw, secret) {
			t.Fatalf("summary leaked content: %s", raw)
		}
	}
	if !bytes.Contains(raw, []byte(`"reasoning_effort":"none"`)) || !bytes.Contains(raw, []byte(`"encrypted_lengths":[22]`)) {
		t.Fatalf("summary omitted safe diagnostics: %s", raw)
	}
}

func TestApplyStateCompatibilityFallbackLeavesUnrelatedBodyUntouched(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"grok-4.6","input":"hello"}`)
	for _, issue := range []stateCompatibilityIssue{issueEncryptedContent, issueReasoningEffort, issueCompactionBlob, ""} {
		normalized, changed := applyStateCompatibilityFallback(body, issue)
		if changed || !bytes.Equal(normalized, body) {
			t.Fatalf("issue %q changed unrelated body: %s", issue, normalized)
		}
	}
}

func decodeStatePayload(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return payload
}
