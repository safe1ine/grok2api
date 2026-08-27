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

func TestNormalizeToolSchemasDoesNotConfusePropertyNamesWithSchemaKeywords(t *testing.T) {
	t.Parallel()

	body := []byte(`{"tools":[{"type":"function","name":"KeywordFields","parameters":{
		"type":"object",
		"properties":{
			"properties":{"type":"string"},
			"required":null,
			"nested":{"type":"object","properties":{"properties":{"type":"boolean"}}}
		}
	}}]}`)
	normalized, changed := normalizeToolSchemas(body)
	if !changed {
		t.Fatal("expected invalid property schema to be normalized")
	}
	schema := decodePayload(t, normalized)["tools"].([]any)[0].(map[string]any)["parameters"].(map[string]any)
	properties := schema["properties"].(map[string]any)
	if _, ok := properties["required"].(map[string]any); !ok {
		t.Fatalf("property named required = %#v, want schema object", properties["required"])
	}
	if _, ok := schema["required"].([]any); !ok {
		t.Fatalf("root required = %#v, want array", schema["required"])
	}
	nested := properties["nested"].(map[string]any)
	nestedProperties := nested["properties"].(map[string]any)
	if _, exists := nestedProperties["required"]; exists {
		t.Fatalf("properties container was polluted with required: %#v", nestedProperties)
	}
}

func TestNormalizeToolSchemasAddsMissingDescriptions(t *testing.T) {
	t.Parallel()

	body := []byte(`{"tools":[
		{"type":"function","name":"Responses","parameters":{"type":"object"}},
		{"name":"Anthropic","input_schema":{"type":"object"}},
		{"type":"function","function":{"name":"Chat","parameters":{"type":"object"}}}
	]}`)
	normalized, changed := normalizeToolSchemas(body)
	if !changed {
		t.Fatal("expected missing descriptions to be added")
	}
	tools := decodePayload(t, normalized)["tools"].([]any)
	if tools[0].(map[string]any)["description"] != fallbackToolDescription {
		t.Fatalf("responses description missing: %#v", tools[0])
	}
	if tools[1].(map[string]any)["description"] != fallbackToolDescription {
		t.Fatalf("anthropic description missing: %#v", tools[1])
	}
	function := tools[2].(map[string]any)["function"].(map[string]any)
	if function["description"] != fallbackToolDescription {
		t.Fatalf("chat description missing: %#v", function)
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
	if _, exists := schema["oneOf"]; exists {
		t.Fatal("root oneOf should be collapsed")
	}
	properties := schema["properties"].(map[string]any)
	for _, name := range []string{"file_path", "edits", "old_string", "new_string"} {
		if _, exists := properties[name]; !exists {
			t.Fatalf("merged properties lost %s", name)
		}
	}
	required := schema["required"].([]any)
	if len(required) != 1 || required[0] != "file_path" {
		t.Fatalf("required = %#v, want only common root requirement", required)
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
	if _, exists := schema["$ref"]; exists {
		t.Fatalf("object $ref was not inlined: %v", schema["$ref"])
	}
	if _, exists := schema["$defs"]; exists {
		t.Fatal("unused $defs should be removed after inlining")
	}
	if _, exists := schema["properties"].(map[string]any)["path"]; !exists {
		t.Fatalf("inlined properties were lost: %#v", schema)
	}
}

func TestNormalizeToolSchemasInlinesNestedDefsReference(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"tools":[{
			"name":"StructuredOutput",
			"input_schema":{
				"type":"object",
				"$defs":{
					"structure_constraint":{
						"type":"object",
						"properties":{"kind":{"type":"string"}},
						"required":["kind"]
					}
				},
				"properties":{
					"type_constraints":{
						"type":"array",
						"items":{"$ref":"#/$defs/structure_constraint"}
					}
				}
			}
		}]
	}`)

	normalized, changed := normalizeToolSchemas(body)
	if !changed {
		t.Fatal("expected local $ref to be inlined")
	}
	payload := decodePayload(t, normalized)
	schema := payload["tools"].([]any)[0].(map[string]any)["input_schema"].(map[string]any)
	if _, exists := schema["$defs"]; exists {
		t.Fatal("unused $defs should be removed")
	}
	items := schema["properties"].(map[string]any)["type_constraints"].(map[string]any)["items"].(map[string]any)
	if _, exists := items["$ref"]; exists {
		t.Fatalf("nested $ref was not inlined: %#v", items)
	}
	if items["type"] != "object" {
		t.Fatalf("items type = %v, want object", items["type"])
	}
	if required, ok := items["required"].([]any); !ok || len(required) != 1 || required[0] != "kind" {
		t.Fatalf("items required = %#v", items["required"])
	}
	if _, exists := items["properties"].(map[string]any)["kind"]; !exists {
		t.Fatalf("inlined item properties = %#v", items)
	}
}

func TestInlineLocalSchemaRefsKeepsRecursiveReferenceFinite(t *testing.T) {
	t.Parallel()

	schema := map[string]any{
		"type": "object",
		"$defs": map[string]any{
			"node": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"child": map[string]any{"$ref": "#/$defs/node"},
				},
			},
		},
		"properties": map[string]any{
			"root": map[string]any{"$ref": "#/$defs/node"},
		},
	}
	if !inlineLocalSchemaRefs(schema) {
		t.Fatal("expected resolvable outer reference to be inlined")
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 16*1024 {
		t.Fatalf("recursive schema expanded unexpectedly: %d bytes", len(encoded))
	}
	if !bytes.Contains(encoded, []byte(`"$ref":"#/$defs/node"`)) {
		t.Fatalf("recursive reference should remain finite and resolvable: %s", encoded)
	}
	if !bytes.Contains(encoded, []byte(`"$defs"`)) {
		t.Fatalf("definitions required by recursive reference were removed: %s", encoded)
	}
}

func TestNormalizeToolSchemasMakesEveryRetainedRootUnionBranchObject(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"tools":[{"name":"automation_update","input_schema":{"anyOf":[
			{"properties":{"id":{"type":"string"}}},
			{"allOf":[{"type":"object","properties":{"patch":{"type":"string"}}}]},
			{"type":"null"}
		]}}]
	}`)
	normalized, changed := normalizeToolSchemas(body)
	if !changed {
		t.Fatal("expected root union to be normalized")
	}
	payload := decodePayload(t, normalized)
	schema := payload["tools"].([]any)[0].(map[string]any)["input_schema"].(map[string]any)
	if schema["type"] != "object" {
		t.Fatalf("root type = %v, want object", schema["type"])
	}
	if _, exists := schema["anyOf"]; exists {
		t.Fatal("root anyOf should be collapsed")
	}
	properties := schema["properties"].(map[string]any)
	for _, name := range []string{"id", "patch"} {
		if _, exists := properties[name]; !exists {
			t.Fatalf("merged properties lost %s", name)
		}
	}
}

func TestNormalizeToolSchemasMergesAlternativeRequiredIntersection(t *testing.T) {
	t.Parallel()

	body := []byte(`{"tools":[{"type":"function","name":"Update","parameters":{"anyOf":[
		{"type":"object","properties":{"common":{"type":"string"},"left":{"type":"string"}},"required":["common","left"]},
		{"type":"object","properties":{"common":{"type":"string"},"right":{"type":"number"}},"required":["common","right"]}
	]}}]}`)
	normalized, changed := normalizeToolSchemas(body)
	if !changed {
		t.Fatal("expected root alternatives to be merged")
	}
	schema := decodePayload(t, normalized)["tools"].([]any)[0].(map[string]any)["parameters"].(map[string]any)
	if _, exists := schema["anyOf"]; exists {
		t.Fatal("root anyOf should be removed")
	}
	properties := schema["properties"].(map[string]any)
	for _, name := range []string{"common", "left", "right"} {
		if _, exists := properties[name]; !exists {
			t.Fatalf("merged properties lost %s", name)
		}
	}
	required := schema["required"].([]any)
	if len(required) != 1 || required[0] != "common" {
		t.Fatalf("required = %#v, want common intersection", required)
	}
}

func TestNormalizeToolSchemasCollapsesAllOfWrappedRootUnion(t *testing.T) {
	t.Parallel()

	body := []byte(`{"tools":[{"type":"function","name":"Update","parameters":{"allOf":[
		{"type":"object","properties":{"base":{"type":"string"}},"required":["base"]},
		{"anyOf":[
			{"type":"object","properties":{"left":{"type":"string"}}},
			{"type":"object","properties":{"right":{"type":"number"}}},
			{"type":"null"}
		]}
	]}}]}`)
	normalized, changed := normalizeToolSchemas(body)
	if !changed {
		t.Fatal("expected wrapped root union to be collapsed")
	}
	schema := decodePayload(t, normalized)["tools"].([]any)[0].(map[string]any)["parameters"].(map[string]any)
	for _, keyword := range []string{"allOf", "anyOf", "oneOf"} {
		if _, exists := schema[keyword]; exists {
			t.Fatalf("root %s should be removed", keyword)
		}
	}
	properties := schema["properties"].(map[string]any)
	for _, name := range []string{"base", "left", "right"} {
		if _, exists := properties[name]; !exists {
			t.Fatalf("merged properties lost %s", name)
		}
	}
	required := schema["required"].([]any)
	if len(required) != 1 || required[0] != "base" {
		t.Fatalf("required = %#v, want allOf requirement", required)
	}
}

func TestNormalizeToolSchemasFallsBackToObjectForScalarRootUnion(t *testing.T) {
	t.Parallel()

	body := []byte(`{"tools":[{"type":"function","name":"Bad","parameters":{"anyOf":[{"type":"string"},{"type":"null"}]}}]}`)
	normalized, changed := normalizeToolSchemas(body)
	if !changed {
		t.Fatal("expected scalar root union to be replaced")
	}
	payload := decodePayload(t, normalized)
	schema := payload["tools"].([]any)[0].(map[string]any)["parameters"].(map[string]any)
	if schema["type"] != "object" {
		t.Fatalf("root type = %v, want object", schema["type"])
	}
	if _, exists := schema["anyOf"]; exists {
		t.Fatalf("scalar root union retained: %#v", schema)
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

func TestRelaxToolParameterRootsKeepsPropertiesAndRemovesConstraints(t *testing.T) {
	t.Parallel()

	body := []byte(`{"tools":[{"type":"function","name":"Update","parameters":{
		"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false,
		"oneOf":[{"required":["value"]}]
	}}]}`)
	relaxed, changed := relaxToolParameterRoots(body)
	if !changed {
		t.Fatal("expected root schema fallback")
	}
	schema := decodePayload(t, relaxed)["tools"].([]any)[0].(map[string]any)["parameters"].(map[string]any)
	if schema["type"] != "object" || schema["additionalProperties"] != true {
		t.Fatalf("relaxed schema = %#v", schema)
	}
	if len(schema["required"].([]any)) != 0 {
		t.Fatalf("required = %#v, want empty", schema["required"])
	}
	if _, exists := schema["oneOf"]; exists {
		t.Fatal("root oneOf should be removed")
	}
	if _, exists := schema["properties"].(map[string]any)["value"]; !exists {
		t.Fatal("known properties should be preserved")
	}
}

func TestNormalizeToolSchemasLeavesUnsupportedAndOrdinaryBodiesUntouched(t *testing.T) {
	t.Parallel()

	tests := [][]byte{
		[]byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"hi"}]}`),
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
	for _, secret := range []string{"SECRET_PROMPT", "SECRET_DESCRIPTION", "SECRET_PROPERTY", "Edit", "path"} {
		if bytes.Contains([]byte(summary), []byte(secret)) {
			t.Fatalf("summary leaked %s: %s", secret, summary)
		}
	}
	for _, expected := range []string{`"format":"parameters"`, `"type":"object"`, `"property_count":1`, `"required_count":1`} {
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
