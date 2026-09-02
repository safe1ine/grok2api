package gateway

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFlattenNamespaceToolsAndRewriteRequestHistory(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model":"grok-4.6",
		"input":[{"type":"function_call","name":"get_customer","namespace":"crm","call_id":"call_1","arguments":"{}"}],
		"tool_choice":{"type":"function","name":"get_customer","namespace":"crm"},
		"tools":[
			{"type":"function","name":"other","parameters":{"type":"object"}},
			{"type":"namespace","name":"crm","description":"CRM tools","tools":[
				{"type":"function","name":"get_customer","description":"Get customer","parameters":{"type":"object","required":null},"strict":true,"allowed_callers":["direct"],"defer_loading":true,"output_schema":{"type":"object"}},
				{"type":"custom","name":"query","format":{"type":"text"}}
			]}
		]
	}`)

	normalized, mappings, changed := flattenNamespaceTools(body)
	if !changed {
		t.Fatal("expected namespace to be flattened")
	}
	mapping, ok := mappings["crm__get_customer"]
	if !ok || mapping.Namespace != "crm" || mapping.Name != "get_customer" {
		t.Fatalf("mappings = %#v", mappings)
	}

	payload := decodeNamespacePayload(t, normalized)
	tools := payload["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("tools length = %d, want 3", len(tools))
	}
	flat := tools[1].(map[string]any)
	if flat["type"] != "function" || flat["name"] != "crm__get_customer" || flat["strict"] != true {
		t.Fatalf("flat tool = %#v", flat)
	}
	if flat["description"] != "CRM tools\nGet customer" {
		t.Fatalf("description = %#v", flat["description"])
	}
	for _, forbidden := range []string{"allowed_callers", "defer_loading", "output_schema"} {
		if _, exists := flat[forbidden]; exists {
			t.Fatalf("flat tool retained unsupported field %q", forbidden)
		}
	}
	custom := tools[2].(map[string]any)
	customMapping, ok := mappings["crm__query"]
	if custom["type"] != "function" || !ok || !customMapping.Custom {
		t.Fatalf("custom tool=%#v mapping=%#v", custom, customMapping)
	}

	call := payload["input"].([]any)[0].(map[string]any)
	if call["name"] != "crm__get_customer" {
		t.Fatalf("history call name = %#v", call["name"])
	}
	if _, exists := call["namespace"]; exists {
		t.Fatal("history call retained namespace")
	}
	choice := payload["tool_choice"].(map[string]any)
	if choice["type"] != "function" || choice["name"] != "crm__get_customer" {
		t.Fatalf("tool choice = %#v", choice)
	}
	if _, exists := choice["namespace"]; exists {
		t.Fatal("tool choice retained namespace")
	}
}

func TestConvertCustomToolRequestAndResponse(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"input":[
			{"type":"custom_tool_call","name":"patch","call_id":"call_1","input":"raw patch"},
			{"type":"custom_tool_call_output","call_id":"call_1","output":"ok"}
		],
		"tool_choice":{"type":"custom","name":"patch"},
		"tools":[{"type":"custom","name":"patch","description":"Apply patch","format":{"type":"text"}}]
	}`)
	normalized, mappings, changed := flattenNamespaceTools(body)
	if !changed {
		t.Fatal("expected custom tool conversion")
	}
	mapping, ok := mappings["patch"]
	if !ok || !mapping.Custom || mapping.Name != "patch" {
		t.Fatalf("mapping = %#v", mapping)
	}
	payload := decodeNamespacePayload(t, normalized)
	tool := payload["tools"].([]any)[0].(map[string]any)
	if tool["type"] != "function" || tool["description"] != "Apply patch" {
		t.Fatalf("converted tool = %#v", tool)
	}
	parameters := tool["parameters"].(map[string]any)
	if parameters["type"] != "object" || len(parameters["required"].([]any)) != 1 {
		t.Fatalf("parameters = %#v", parameters)
	}
	input := payload["input"].([]any)
	call := input[0].(map[string]any)
	if call["type"] != "function_call" || customInputFromArguments(call["arguments"]) != "raw patch" {
		t.Fatalf("converted history call = %#v", call)
	}
	if input[1].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("converted history output = %#v", input[1])
	}
	choice := payload["tool_choice"].(map[string]any)
	if choice["type"] != "function" || choice["name"] != "patch" {
		t.Fatalf("converted choice = %#v", choice)
	}

	response := []byte(`{"output":[{"type":"function_call","name":"patch","call_id":"call_1","arguments":"{\"input\":\"raw patch\"}"}]}`)
	rewritten, changed := rewriteNamespaceToolCalls(response, mappings)
	if !changed {
		t.Fatal("expected custom response conversion")
	}
	item := decodeNamespacePayload(t, rewritten)["output"].([]any)[0].(map[string]any)
	if item["type"] != "custom_tool_call" || item["input"] != "raw patch" {
		t.Fatalf("rewritten custom call = %#v", item)
	}
}

func TestConvertCustomToolStreamingEvents(t *testing.T) {
	t.Parallel()

	mappings := namespaceToolMappings{"patch": {Name: "patch", Custom: true}}
	input := `data: {"type":"response.output_item.added","item":{"id":"item_1","type":"function_call","name":"patch","call_id":"call_1","arguments":""}}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.done","item_id":"item_1","arguments":"{\"input\":\"raw patch\"}"}` + "\n\n"
	recorder := httptest.NewRecorder()
	streamCopyWithNamespaceMappings(recorder, strings.NewReader(input), "text/event-stream", mappings, time.Now())
	output := recorder.Body.String()
	for _, expected := range []string{
		`"type":"custom_tool_call"`,
		`"type":"response.custom_tool_call_input.delta"`,
		`"delta":"raw patch"`,
		`"type":"response.custom_tool_call_input.done"`,
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("stream output missing %s: %s", expected, output)
		}
	}
	if strings.Contains(output, "response.function_call_arguments.done") {
		t.Fatalf("function event leaked to custom client: %s", output)
	}
}

func TestPromoteAdditionalAndToolSearchTools(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"input":[
			{"type":"additional_tools","role":"developer","tools":[
				{"type":"namespace","name":"crm","description":"CRM","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]},
				{"type":"function","name":"dynamic","parameters":{"type":"object"}}
			]},
			{"type":"tool_search_call","call_id":"search_1","arguments":{"query":"tools"}},
			{"type":"tool_search_output","call_id":"search_1","tools":[{"type":"function","name":"searched","parameters":{"type":"object"}}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
		],
		"tools":[{"type":"namespace","name":"crm","description":"CRM","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}]
	}`)

	normalized, mappings, changed := flattenNamespaceTools(body)
	if !changed {
		t.Fatal("expected discovery items to be promoted")
	}
	if _, ok := mappings["crm__lookup"]; !ok {
		t.Fatalf("namespace mapping missing: %#v", mappings)
	}
	payload := decodeNamespacePayload(t, normalized)
	input := payload["input"].([]any)
	if len(input) != 1 || input[0].(map[string]any)["type"] != "message" {
		t.Fatalf("filtered input = %#v", input)
	}
	tools := payload["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("promoted tools length = %d, want 3: %#v", len(tools), tools)
	}
	names := map[string]int{}
	for _, rawTool := range tools {
		name, _ := rawTool.(map[string]any)["name"].(string)
		names[name]++
	}
	for _, name := range []string{"crm__lookup", "dynamic", "searched"} {
		if names[name] != 1 {
			t.Fatalf("tool %s count = %d; all=%#v", name, names[name], names)
		}
	}
}

func TestPromoteAdditionalToolsWithoutNamespace(t *testing.T) {
	t.Parallel()

	body := []byte(`{"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"find","parameters":{"type":"object"}}]},{"type":"message","role":"user","content":"hi"}]}`)
	normalized, mappings, changed := flattenNamespaceTools(body)
	if !changed || len(mappings) != 0 {
		t.Fatalf("changed=%t mappings=%#v", changed, mappings)
	}
	payload := decodeNamespacePayload(t, normalized)
	if len(payload["input"].([]any)) != 1 || len(payload["tools"].([]any)) != 1 {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestFlattenNamespaceToolsAvoidsNameCollisions(t *testing.T) {
	t.Parallel()

	body := []byte(`{"tools":[
		{"type":"function","name":"crm__find","parameters":{"type":"object"}},
		{"type":"namespace","name":"crm","description":"","tools":[{"type":"function","name":"find","parameters":{"type":"object"}}]}
	]}`)
	normalized, mappings, changed := flattenNamespaceTools(body)
	if !changed {
		t.Fatal("expected namespace to be flattened")
	}
	if _, ok := mappings["crm__find_2"]; !ok {
		t.Fatalf("mappings = %#v", mappings)
	}
	payload := decodeNamespacePayload(t, normalized)
	flat := payload["tools"].([]any)[1].(map[string]any)
	if flat["name"] != "crm__find_2" {
		t.Fatalf("flat name = %#v", flat["name"])
	}
}

func TestFlattenNamespaceToolsLeavesOrdinaryRequestUntouched(t *testing.T) {
	t.Parallel()

	body := []byte(`{"tools":[{"type":"function","name":"find","description":"Find","parameters":{"type":"object"}}]}`)
	normalized, mappings, changed := flattenNamespaceTools(body)
	if changed || len(mappings) != 0 || !bytes.Equal(normalized, body) {
		t.Fatalf("ordinary request changed: %s %#v", normalized, mappings)
	}
}

func TestNormalizeOptionalToolChoiceWithoutTools(t *testing.T) {
	t.Parallel()

	for _, body := range []string{
		`{"model":"grok-4.6","tool_choice":"auto"}`,
		`{"model":"grok-4.6","tool_choice":"none","tools":[]}`,
		`{"model":"grok-4.6","tool_choice":{"type":"auto"},"tools":[]}`,
		`{"model":"grok-4.6","tool_choice":null,"tools":[]}`,
	} {
		normalized, changed, invalid := normalizeToolChoiceWithoutTools([]byte(body))
		if !changed || invalid {
			t.Fatalf("body=%s changed=%t invalid=%t", body, changed, invalid)
		}
		payload := decodeNamespacePayload(t, normalized)
		if _, exists := payload["tool_choice"]; exists {
			t.Fatalf("tool_choice retained: %s", normalized)
		}
		if _, exists := payload["tools"]; exists {
			t.Fatalf("empty tools retained: %s", normalized)
		}
	}
}

func TestNormalizeRequiredToolChoiceWithoutToolsIsInvalid(t *testing.T) {
	t.Parallel()

	for _, body := range []string{
		`{"tool_choice":"required"}`,
		`{"tool_choice":{"type":"function","name":"lookup"},"tools":[]}`,
		`{"tool_choice":{"type":"tool","name":"lookup"},"tools":[]}`,
		`{"tool_choice":{"type":"any"},"tools":[]}`,
	} {
		normalized, changed, invalid := normalizeToolChoiceWithoutTools([]byte(body))
		if changed || !invalid || !bytes.Equal(normalized, []byte(body)) {
			t.Fatalf("body=%s normalized=%s changed=%t invalid=%t", body, normalized, changed, invalid)
		}
	}
}

func TestNormalizeToolChoiceKeepsRequestsWithTools(t *testing.T) {
	t.Parallel()

	body := []byte(`{"tool_choice":"required","tools":[{"type":"function","name":"lookup"}]}`)
	normalized, changed, invalid := normalizeToolChoiceWithoutTools(body)
	if changed || invalid || !bytes.Equal(normalized, body) {
		t.Fatalf("normalized=%s changed=%t invalid=%t", normalized, changed, invalid)
	}
}

func TestNormalizeToolChoiceAfterEmptyNamespaceFlatten(t *testing.T) {
	t.Parallel()

	body := []byte(`{"tool_choice":"auto","tools":[{"type":"namespace","name":"empty","tools":[]}]}`)
	flattened, _, changed := flattenNamespaceTools(body)
	if !changed {
		t.Fatal("expected empty namespace to be flattened")
	}
	normalized, choiceChanged, invalid := normalizeToolChoiceWithoutTools(flattened)
	if !choiceChanged || invalid {
		t.Fatalf("normalized=%s changed=%t invalid=%t", normalized, choiceChanged, invalid)
	}
	payload := decodeNamespacePayload(t, normalized)
	if _, exists := payload["tool_choice"]; exists {
		t.Fatalf("tool_choice retained: %s", normalized)
	}
	if _, exists := payload["tools"]; exists {
		t.Fatalf("empty tools retained: %s", normalized)
	}
}

func TestRewriteNamespaceToolCallsJSON(t *testing.T) {
	t.Parallel()

	mappings := namespaceToolMappings{
		"crm__get_customer": {Namespace: "crm", Name: "get_customer"},
	}
	body := []byte(`{"id":"resp_1","output":[{"type":"function_call","name":"crm__get_customer","call_id":"call_1","arguments":"{}"}]}`)
	rewritten, changed := rewriteNamespaceToolCalls(body, mappings)
	if !changed {
		t.Fatal("expected response call to be rewritten")
	}
	payload := decodeNamespacePayload(t, rewritten)
	call := payload["output"].([]any)[0].(map[string]any)
	if call["name"] != "get_customer" || call["namespace"] != "crm" {
		t.Fatalf("rewritten call = %#v", call)
	}
}

func TestStreamCopyNamespaceSSE(t *testing.T) {
	t.Parallel()

	mappings := namespaceToolMappings{
		"crm__get_customer": {Namespace: "crm", Name: "get_customer"},
	}
	input := "event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","item":{"type":"function_call","name":"crm__get_customer","call_id":"call_1"}}` + "\n\n" +
		"data: [DONE]\n\n"
	recorder := httptest.NewRecorder()
	streamCopyWithNamespaceMappings(recorder, strings.NewReader(input), "text/event-stream", mappings, time.Now())
	output := recorder.Body.String()
	if !strings.Contains(output, `"name":"get_customer"`) || !strings.Contains(output, `"namespace":"crm"`) {
		t.Fatalf("SSE output = %s", output)
	}
	if !strings.Contains(output, "data: [DONE]") {
		t.Fatalf("SSE lost done marker: %s", output)
	}
}

func TestStreamCopyFillsMissingAnthropicIndexes(t *testing.T) {
	t.Parallel()

	input := "event: message_start\n" +
		`data: {"type":"message_start","message":{"content":[]}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"one"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"thinking_delta","thinking":"two"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop"}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\n"

	recorder := httptest.NewRecorder()
	streamCopyWithCompatibility(
		recorder, strings.NewReader(input), "text/event-stream", nil, time.Now(),
		streamCompatibilityOptions{fillAnthropicIndexes: true},
	)
	payloads := decodeSSEPayloads(t, recorder.Body.String())
	if len(payloads) != 8 {
		t.Fatalf("payload count = %d, output = %s", len(payloads), recorder.Body.String())
	}
	assertNoSSEIndex(t, payloads[0])
	assertSSEIndex(t, payloads[1], 2)
	assertSSEIndex(t, payloads[2], 2)
	assertSSEIndex(t, payloads[3], 2)
	assertSSEIndex(t, payloads[4], 2)
	assertSSEIndex(t, payloads[5], 3)
	assertSSEIndex(t, payloads[6], 3)
	assertNoSSEIndex(t, payloads[7])
}

func TestStreamCopyRenumbersRepeatedAnthropicIndexes(t *testing.T) {
	t.Parallel()

	input := "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"one"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"lookup","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_2","name":"search","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n"

	recorder := httptest.NewRecorder()
	streamCopyWithCompatibility(
		recorder, strings.NewReader(input), "text/event-stream", nil, time.Now(),
		streamCompatibilityOptions{fillAnthropicIndexes: true},
	)
	payloads := decodeSSEPayloads(t, recorder.Body.String())
	if len(payloads) != 9 {
		t.Fatalf("payload count = %d, output = %s", len(payloads), recorder.Body.String())
	}
	for i, want := range []int{0, 0, 0, 1, 1, 1, 2, 2, 2} {
		assertSSEIndex(t, payloads[i], want)
	}
}

func TestStreamCopyDropsMergedAnthropicToolCallsAfterFirst(t *testing.T) {
	t.Parallel()

	input := "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"bash","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"pwd\"}"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"/tmp/a.txt\"}"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"/tmp/b.txt\",\"content\":\"12345\"}"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n"

	recorder := httptest.NewRecorder()
	streamCopyWithCompatibility(
		recorder, strings.NewReader(input), "text/event-stream", nil, time.Now(),
		streamCompatibilityOptions{fillAnthropicIndexes: true},
	)
	payloads := decodeSSEPayloads(t, recorder.Body.String())
	if len(payloads) != 7 {
		t.Fatalf("payload count = %d, output = %s", len(payloads), recorder.Body.String())
	}
	for i, want := range []int{0, 0, 1, 1, 2, 2, 2} {
		assertSSEIndex(t, payloads[i], want)
	}
	firstBlock := payloads[4]["content_block"].(map[string]any)
	firstDelta := payloads[5]["delta"].(map[string]any)
	if firstBlock["name"] != "bash" || firstDelta["partial_json"] != `{"command":"pwd"}` {
		t.Fatalf("first tool = %#v arguments = %v", firstBlock, firstDelta["partial_json"])
	}
	output := recorder.Body.String()
	if strings.Contains(output, "/tmp/a.txt") || strings.Contains(output, "/tmp/b.txt") || strings.Contains(output, "split-") {
		t.Fatalf("later merged tools were not dropped: %s", output)
	}
	assertCanonicalSSE(t, output)
}

func TestStreamCopyKeepsFragmentedAnthropicToolInputTogether(t *testing.T) {
	t.Parallel()

	input := "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"bash","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"pwd\"}"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n"

	recorder := httptest.NewRecorder()
	streamCopyWithCompatibility(
		recorder, strings.NewReader(input), "text/event-stream", nil, time.Now(),
		streamCompatibilityOptions{fillAnthropicIndexes: true},
	)
	payloads := decodeSSEPayloads(t, recorder.Body.String())
	if len(payloads) != 4 {
		t.Fatalf("payload count = %d, output = %s", len(payloads), recorder.Body.String())
	}
	for _, payload := range payloads {
		assertSSEIndex(t, payload, 0)
	}
	first := payloads[1]["delta"].(map[string]any)["partial_json"].(string)
	second := payloads[2]["delta"].(map[string]any)["partial_json"].(string)
	if first+second != `{"command":"pwd"}` {
		t.Fatalf("arguments = %q", first+second)
	}
}

func TestStreamCopyNormalizesAnthropicUsageWithoutChangingMetrics(t *testing.T) {
	t.Parallel()

	input := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":4497,"cache_creation_input_tokens":0,"cache_read_input_tokens":4352,"output_tokens":0}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":187}}` + "\n\n"

	recorder := httptest.NewRecorder()
	metrics := streamCopyWithCompatibility(
		recorder, strings.NewReader(input), "text/event-stream", nil, time.Now(),
		streamCompatibilityOptions{normalizeAnthropicUsage: true},
	)
	payloads := decodeSSEPayloads(t, recorder.Body.String())
	message := payloads[0]["message"].(map[string]any)
	usage := message["usage"].(map[string]any)
	if inputTokens, ok := tokenCount(usage["input_tokens"]); !ok || inputTokens != 145 {
		t.Fatalf("forwarded usage = %#v", usage)
	}
	if metrics.InputTokens != 4497 || metrics.CachedInputTokens != 4352 || metrics.OutputTokens != 187 {
		t.Fatalf("internal metrics = %+v", metrics)
	}
}

func TestNonStreamCopyNormalizesAnthropicCacheCreationUsage(t *testing.T) {
	t.Parallel()

	input := `{"type":"message","usage":{"input_tokens":100,"cache_creation_input_tokens":20,"cache_read_input_tokens":60,"output_tokens":10}}`
	recorder := httptest.NewRecorder()
	metrics := streamCopyWithCompatibility(
		recorder, strings.NewReader(input), "application/json", nil, time.Now(),
		streamCompatibilityOptions{normalizeAnthropicUsage: true},
	)
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	usage := payload["usage"].(map[string]any)
	if usage["input_tokens"] != float64(20) {
		t.Fatalf("forwarded usage = %#v", usage)
	}
	if metrics.InputTokens != 100 || metrics.CachedInputTokens != 60 || metrics.OutputTokens != 10 {
		t.Fatalf("internal metrics = %+v", metrics)
	}
}

func TestStreamCopyDoesNotFillAnthropicIndexesByDefault(t *testing.T) {
	t.Parallel()

	input := "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}` + "\n\n"
	recorder := httptest.NewRecorder()
	streamCopyWithNamespaceMappings(recorder, strings.NewReader(input), "text/event-stream", nil, time.Now())
	payloads := decodeSSEPayloads(t, recorder.Body.String())
	assertNoSSEIndex(t, payloads[1])
}

func TestUniqueNamespaceToolNameIsSafeAndBounded(t *testing.T) {
	t.Parallel()

	name := uniqueNamespaceToolName("命名空间/with dots", strings.Repeat("child", 20), map[string]bool{})
	if len(name) > 64 {
		t.Fatalf("name length = %d", len(name))
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '_' && r != '-' {
			t.Fatalf("unsafe character %q in %q", r, name)
		}
	}
}

func assertCanonicalSSE(t *testing.T, stream string) {
	t.Helper()
	stream = strings.ReplaceAll(stream, "\r\n", "\n")
	if strings.Contains(stream, "\n\n\n") {
		t.Fatalf("stream contains extra blank lines: %s", stream)
	}
	for _, block := range strings.Split(strings.TrimSpace(stream), "\n\n") {
		lines := strings.Split(block, "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("non-canonical SSE block %q in %s", block, stream)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(lines[1], "data:"))), &payload); err != nil {
			t.Fatalf("decode SSE block %q: %v", block, err)
		}
		if eventType, _ := payload["type"].(string); strings.TrimSpace(strings.TrimPrefix(lines[0], "event:")) != eventType {
			t.Fatalf("event label does not match payload: %q", block)
		}
	}
}

func decodeSSEPayloads(t *testing.T, stream string) []map[string]any {
	t.Helper()
	var payloads []map[string]any
	for _, line := range strings.Split(stream, "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(strings.TrimPrefix(line, "data:"))))
		dec.UseNumber()
		var payload map[string]any
		if err := dec.Decode(&payload); err != nil {
			t.Fatalf("decode SSE payload %q: %v", line, err)
		}
		payloads = append(payloads, payload)
	}
	return payloads
}

func assertSSEIndex(t *testing.T, payload map[string]any, want int) {
	t.Helper()
	index, ok := anthropicEventIndex(payload)
	if !ok || index != want {
		t.Fatalf("%s index = %v, want %d", payload["type"], payload["index"], want)
	}
}

func assertNoSSEIndex(t *testing.T, payload map[string]any) {
	t.Helper()
	if _, exists := payload["index"]; exists {
		t.Fatalf("%s unexpectedly has index %v", payload["type"], payload["index"])
	}
}

func decodeNamespacePayload(t *testing.T, body []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var payload map[string]any
	if err := dec.Decode(&payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return payload
}
