package gateway

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestNormalizeToolSchemasAnthropic(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model":"grok-4.6",
		"max_tokens":9007199254740993,
		"tools":[{
			"name":"Edit",
			"input_schema":{
				"type":"object",
				"required":null,
				"properties":{"options":{"type":"object","required":null}}
			}
		}]
	}`)
	normalized, changed := normalizeToolSchemas(body)
	if !changed {
		t.Fatal("expected schema to be normalized")
	}

	payload := decodePayload(t, normalized)
	if got := payload["max_tokens"].(json.Number).String(); got != "9007199254740993" {
		t.Fatalf("max_tokens changed to %s", got)
	}
	schema := payload["tools"].([]any)[0].(map[string]any)["input_schema"].(map[string]any)
	if required, ok := schema["required"].([]any); !ok || len(required) != 0 {
		t.Fatalf("root required = %#v, want empty array", schema["required"])
	}
	nested := schema["properties"].(map[string]any)["options"].(map[string]any)
	if required, ok := nested["required"].([]any); !ok || len(required) != 0 {
		t.Fatalf("nested required = %#v, want empty array", nested["required"])
	}
}

func TestNormalizeToolSchemasAddsRequiredArraysToObjects(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"tools":[{"name":"NoRequired","input_schema":{"type":"object","properties":{
			"options":{"type":["object","null"],"properties":{"enabled":{"type":"boolean"}}},
			"label":{"type":"string"}
		}}}]
	}`)
	normalized, changed := normalizeToolSchemas(body)
	if !changed {
		t.Fatal("expected missing required arrays to be added")
	}
	payload := decodePayload(t, normalized)
	schema := payload["tools"].([]any)[0].(map[string]any)["input_schema"].(map[string]any)
	if required, ok := schema["required"].([]any); !ok || len(required) != 0 {
		t.Fatalf("root required = %#v, want empty array", schema["required"])
	}
	nested := schema["properties"].(map[string]any)["options"].(map[string]any)
	if required, ok := nested["required"].([]any); !ok || len(required) != 0 {
		t.Fatalf("nested required = %#v, want empty array", nested["required"])
	}
}

func TestNormalizeToolSchemasOpenAIResponses(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"tools":[{
			"type":"function",
			"name":"Edit",
			"parameters":{"anyOf":[
				{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]},
				{"type":"null"}
			]}
		}]
	}`)
	normalized, changed := normalizeToolSchemas(body)
	if !changed {
		t.Fatal("expected nullable root union to be normalized")
	}

	payload := decodePayload(t, normalized)
	schema := payload["tools"].([]any)[0].(map[string]any)["parameters"].(map[string]any)
	if schema["type"] != "object" {
		t.Fatalf("root type = %v, want object", schema["type"])
	}
	if _, exists := schema["anyOf"]; exists {
		t.Fatal("single object branch should be flattened")
	}
	if _, exists := schema["properties"].(map[string]any)["path"]; !exists {
		t.Fatal("object branch properties were lost")
	}
}

func TestNormalizeToolSchemasOpenAIResponsesObjectRootConstraintBranches(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"tools":[{
			"type":"function",
			"name":"Edit",
			"parameters":{
				"type":"object",
				"properties":{"file_path":{"type":"string"},"edits":{"type":"array"},"old_string":{"type":"string"},"new_string":{"type":"string"}},
				"required":["file_path"],
				"oneOf":[
					{"required":["old_string","new_string"],"not":{"required":["edits"]}},
					{"required":["edits"],"not":{"required":["old_string","new_string"]}}
				]
			}
		}]
	}`)
	normalized, changed := normalizeToolSchemas(body)
	if !changed {
		t.Fatal("expected object type to be added to root constraint branches")
	}

	payload := decodePayload(t, normalized)
	schema := payload["tools"].([]any)[0].(map[string]any)["parameters"].(map[string]any)
	branches := schema["oneOf"].([]any)
	if len(branches) != 2 {
		t.Fatalf("oneOf branches = %d, want 2", len(branches))
	}
	for i, rawBranch := range branches {
		branch := rawBranch.(map[string]any)
		if branch["type"] != "object" {
			t.Fatalf("branch %d type = %v, want object", i, branch["type"])
		}
		if _, exists := branch["required"]; !exists {
			t.Fatalf("branch %d lost required constraint", i)
		}
		if _, exists := branch["not"]; !exists {
			t.Fatalf("branch %d lost not constraint", i)
		}
	}
}

func TestNormalizeToolSchemasOpenAIResponsesDropsUnknownRootBranch(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"tools":[{
			"type":"function",
			"name":"Edit",
			"parameters":{
				"$defs":{"EditArgs":{"type":"object","properties":{"path":{"type":"string"}}}},
				"anyOf":[{"$ref":"#/$defs/EditArgs"},{}]
			}
		}]
	}`)
	normalized, changed := normalizeToolSchemas(body)
	if !changed {
		t.Fatal("expected unknown root union branch to be removed")
	}

	payload := decodePayload(t, normalized)
	schema := payload["tools"].([]any)[0].(map[string]any)["parameters"].(map[string]any)
	if schema["type"] != "object" {
		t.Fatalf("root type = %v, want object", schema["type"])
	}
	if _, exists := schema["anyOf"]; exists {
		t.Fatal("root anyOf should be flattened")
	}
	if schema["$ref"] != "#/$defs/EditArgs" {
		t.Fatalf("object $ref branch was lost: %v", schema["$ref"])
	}
}

func TestNormalizeToolSchemasOpenAIChat(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"tools":[{
			"type":"function",
			"function":{"name":"Edit","parameters":{"oneOf":[
				{"type":"object","properties":{"path":{"type":"string"}}},
				{"type":"string"}
			]}}
		}]
	}`)
	normalized, changed := normalizeToolSchemas(body)
	if !changed {
		t.Fatal("expected non-object union branch to be removed")
	}

	payload := decodePayload(t, normalized)
	function := payload["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	schema := function["parameters"].(map[string]any)
	if schema["type"] != "object" {
		t.Fatalf("root type = %v, want object", schema["type"])
	}
	if _, exists := schema["oneOf"]; exists {
		t.Fatal("single object branch should be flattened")
	}
}

func TestNormalizeToolSchemasRemovesRequiredNullOutsideTools(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model":"grok-4.6",
		"output_config":{"format":{"type":"json_schema","schema":{"type":"object","required":null,"properties":{"result":{"type":"string"}}}}},
		"response_format":{"json_schema":{"schema":{"type":"object","properties":{"nested":{"type":"object","required":null}}}}}
	}`)
	normalized, changed := normalizeToolSchemas(body)
	if !changed {
		t.Fatal("expected required:null outside tools to be removed")
	}
	payload := decodePayload(t, normalized)
	format := payload["output_config"].(map[string]any)["format"].(map[string]any)
	outputSchema := format["schema"].(map[string]any)
	if required, ok := outputSchema["required"].([]any); !ok || len(required) != 0 {
		t.Fatalf("output_config required = %#v, want empty array", outputSchema["required"])
	}
	responseSchema := payload["response_format"].(map[string]any)["json_schema"].(map[string]any)["schema"].(map[string]any)
	nested := responseSchema["properties"].(map[string]any)["nested"].(map[string]any)
	if required, ok := nested["required"].([]any); !ok || len(required) != 0 {
		t.Fatalf("nested response required = %#v, want empty array", nested["required"])
	}
}

func TestNormalizeToolSchemasLeavesUnsupportedAndOrdinaryBodiesUntouched(t *testing.T) {
	t.Parallel()

	tests := [][]byte{
		[]byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"hi"}]}`),
		[]byte(`{"tools":[{"type":"function","name":"Bad","parameters":{"anyOf":[{"type":"string"},{"type":"null"}]}}]}`),
		[]byte(`not-json`),
	}
	for _, body := range tests {
		normalized, changed := normalizeToolSchemas(body)
		if changed {
			t.Fatalf("unexpected normalization for %s", body)
		}
		if !bytes.Equal(normalized, body) {
			t.Fatalf("body changed: got %s, want %s", normalized, body)
		}
	}
}

func TestToolSchemaSummaryExcludesPromptAndDescriptions(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"input":"SECRET_PROMPT",
		"tools":[{"type":"function","name":"Edit","description":"SECRET_DESCRIPTION","parameters":{"type":"object","properties":{"path":{"type":"string","description":"SECRET_PROPERTY"}},"required":["path"]}}]
	}`)
	summary := toolSchemaSummary(body)
	for _, secret := range []string{"SECRET_PROMPT", "SECRET_DESCRIPTION", "SECRET_PROPERTY"} {
		if bytes.Contains([]byte(summary), []byte(secret)) {
			t.Fatalf("summary leaked %s: %s", secret, summary)
		}
	}
	for _, expected := range []string{"Edit", "parameters", "object", "path"} {
		if !bytes.Contains([]byte(summary), []byte(expected)) {
			t.Fatalf("summary missing %s: %s", expected, summary)
		}
	}
}

func decodePayload(t *testing.T, body []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var payload map[string]any
	if err := dec.Decode(&payload); err != nil {
		t.Fatalf("decode normalized payload: %v", err)
	}
	return payload
}
