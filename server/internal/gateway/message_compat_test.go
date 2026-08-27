package gateway

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestNormalizeAnthropicToolChoiceDisablesToolsForNone(t *testing.T) {
	t.Parallel()

	for _, body := range [][]byte{
		[]byte(`{"tool_choice":{"type":"none"},"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`),
		[]byte(`{"tool_choice":"none","tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`),
	} {
		normalized, changed := normalizeAnthropicToolChoice(body)
		if !changed {
			t.Fatalf("expected none to be normalized: %s", body)
		}
		payload := decodeMessagePayload(t, normalized)
		if _, exists := payload["tool_choice"]; exists {
			t.Fatalf("tool_choice retained: %s", normalized)
		}
		if _, exists := payload["tools"]; exists {
			t.Fatalf("tools retained when disabled: %s", normalized)
		}
	}
}

func TestNormalizeAnthropicToolChoiceConvertsOpenAIVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		body     string
		wantType string
		wantName string
	}{
		{`{"tool_choice":"auto"}`, "auto", ""},
		{`{"tool_choice":{"type":"required","disable_parallel_tool_use":true}}`, "any", ""},
		{`{"tool_choice":{"type":"function","name":"lookup"}}`, "tool", "lookup"},
		{`{"tool_choice":{"type":"function","function":{"name":"search"}}}`, "tool", "search"},
	}
	for _, test := range tests {
		normalized, changed := normalizeAnthropicToolChoice([]byte(test.body))
		if !changed {
			t.Fatalf("expected tool choice to be normalized: %s", test.body)
		}
		choice := decodeMessagePayload(t, normalized)["tool_choice"].(map[string]any)
		if choice["type"] != test.wantType {
			t.Fatalf("body=%s type=%v, want %s", test.body, choice["type"], test.wantType)
		}
		if test.wantName != "" && choice["name"] != test.wantName {
			t.Fatalf("body=%s name=%v, want %s", test.body, choice["name"], test.wantName)
		}
	}
}

func TestNormalizeAnthropicToolChoiceLeavesValidChoiceUntouched(t *testing.T) {
	t.Parallel()

	body := []byte(`{"tool_choice":{"type":"auto"},"tools":[{"name":"lookup"}]}`)
	normalized, changed := normalizeAnthropicToolChoice(body)
	if changed || !bytes.Equal(normalized, body) {
		t.Fatalf("valid choice changed: %s", normalized)
	}
}

func TestNormalizeAnthropicMessageRolesPromotesSystemAndDeveloper(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model":"grok-4.6",
		"system":"base",
		"messages":[
			{"role":"system","content":"system instruction"},
			{"role":"user","content":"hello"},
			{"role":"developer","content":[{"type":"text","text":"developer instruction","cache_control":{"type":"ephemeral"}}]},
			{"role":"assistant","content":"hi"}
		]
	}`)
	normalized, changed := normalizeAnthropicMessageRoles(body)
	if !changed {
		t.Fatal("expected message roles to be normalized")
	}

	payload := decodeMessagePayload(t, normalized)
	messages := payload["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages length = %d, want 2", len(messages))
	}
	if role := messages[0].(map[string]any)["role"]; role != "user" {
		t.Fatalf("first role = %v, want user", role)
	}
	if role := messages[1].(map[string]any)["role"]; role != "assistant" {
		t.Fatalf("second role = %v, want assistant", role)
	}

	system := payload["system"].([]any)
	if len(system) != 3 {
		t.Fatalf("system blocks length = %d, want 3", len(system))
	}
	if text := system[0].(map[string]any)["text"]; text != "base" {
		t.Fatalf("first system block text = %v", text)
	}
	if _, ok := system[2].(map[string]any)["cache_control"]; !ok {
		t.Fatal("developer system block lost cache_control")
	}
}

func TestNormalizeAnthropicMessageRolesConvertsToolHistory(t *testing.T) {
	t.Parallel()

	body := []byte(`{"messages":[{"role":"tool","tool_call_id":"call_1","name":"lookup","content":"ok"}]}`)
	normalized, changed := normalizeAnthropicMessageRoles(body)
	if !changed {
		t.Fatal("expected tool role to be normalized")
	}
	payload := decodeMessagePayload(t, normalized)
	message := payload["messages"].([]any)[0].(map[string]any)
	if message["role"] != "user" {
		t.Fatalf("role = %v, want user", message["role"])
	}
	block := message["content"].([]any)[0].(map[string]any)
	if block["type"] != "tool_result" || block["tool_use_id"] != "call_1" || block["content"] != "ok" {
		t.Fatalf("unexpected tool result: %#v", block)
	}
	if _, exists := message["tool_call_id"]; exists {
		t.Fatal("tool_call_id was not removed")
	}
}

func TestNormalizeAnthropicMessageRolesLeavesValidRequestUntouched(t *testing.T) {
	t.Parallel()

	body := []byte(`{"system":[{"type":"text","text":"system"}],"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"}]}`)
	normalized, changed := normalizeAnthropicMessageRoles(body)
	if changed {
		t.Fatalf("valid request changed: %s", normalized)
	}
	if !bytes.Equal(normalized, body) {
		t.Fatalf("body changed: %s", normalized)
	}
}

func TestMessageRoleSummaryDoesNotContainContent(t *testing.T) {
	t.Parallel()

	body := []byte(`{"system":"secret-system","messages":[{"role":"user","content":"secret-user"},{"role":"assistant","content":[{"type":"tool_use","id":"id","name":"secret-tool","input":{"secret":"argument"}}]}]}`)
	raw, err := json.Marshal(messageRoleSummary(body))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range [][]byte{[]byte("secret-system"), []byte("secret-user"), []byte("secret-tool"), []byte("argument")} {
		if bytes.Contains(raw, secret) {
			t.Fatalf("summary leaked request content: %s", raw)
		}
	}
	if !bytes.Contains(raw, []byte("tool_use")) {
		t.Fatalf("summary omitted content type: %s", raw)
	}
}

func decodeMessagePayload(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return payload
}
