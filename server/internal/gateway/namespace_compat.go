package gateway

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
)

type namespaceToolMapping struct {
	Namespace string
	Name      string
	Custom    bool
}

type namespaceToolMappings map[string]namespaceToolMapping

// flattenNamespaceTools 将 OpenAI Responses 的 namespace function 子工具展开成
// xAI 支持的顶层 function，并返回用于双向改写 function_call 的名称映射。
func flattenNamespaceTools(body []byte) ([]byte, namespaceToolMappings, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var payload map[string]any
	if err := dec.Decode(&payload); err != nil {
		return body, nil, false
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return body, nil, false
	}

	discoveryChanged := promoteDiscoveredTools(payload)
	tools, ok := payload["tools"].([]any)
	if !ok {
		if !discoveryChanged {
			return body, nil, false
		}
		tools = []any{}
	}

	usedNames := map[string]bool{}
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		if toolType, _ := tool["type"].(string); toolType == "function" {
			if name, ok := tool["name"].(string); ok {
				usedNames[name] = true
			}
		}
	}

	flattened := make([]any, 0, len(tools))
	mappings := namespaceToolMappings{}
	seenNamespaceChildren := map[string]bool{}
	changed := discoveryChanged
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			flattened = append(flattened, rawTool)
			continue
		}
		toolType, _ := tool["type"].(string)
		switch toolType {
		case "function":
			if function, ok := tool["function"].(map[string]any); ok {
				changed = ensureFunctionDescription(function, "") || changed
			} else {
				changed = ensureFunctionDescription(tool, "") || changed
			}
			flattened = append(flattened, tool)
			continue
		case "custom":
			name, _ := tool["name"].(string)
			if name == "" {
				changed = true
				continue
			}
			flatName := uniqueNamespaceToolName("", name, usedNames)
			usedNames[flatName] = true
			mappings[flatName] = namespaceToolMapping{Name: name, Custom: true}
			flattened = append(flattened, customToolAsFunction(tool, flatName, ""))
			changed = true
			continue
		case "namespace":
			// 展平后继续处理 namespace 子工具。
		default:
			flattened = append(flattened, tool)
			continue
		}

		changed = true
		namespace, _ := tool["name"].(string)
		namespaceDescription, _ := tool["description"].(string)
		children, _ := tool["tools"].([]any)
		for _, rawChild := range children {
			child, ok := rawChild.(map[string]any)
			if !ok {
				continue
			}
			childType, _ := child["type"].(string)
			if childType != "function" && childType != "custom" {
				continue
			}
			childName, _ := child["name"].(string)
			if childName == "" {
				continue
			}
			semanticName := namespace + "\x00" + childName
			if seenNamespaceChildren[semanticName] {
				continue
			}
			seenNamespaceChildren[semanticName] = true
			flatName := uniqueNamespaceToolName(namespace, childName, usedNames)
			usedNames[flatName] = true
			mapping := namespaceToolMapping{Namespace: namespace, Name: childName, Custom: childType == "custom"}
			mappings[flatName] = mapping
			if mapping.Custom {
				flattened = append(flattened, customToolAsFunction(child, flatName, namespaceDescription))
			} else {
				flattened = append(flattened, namespacedFunctionTool(child, flatName, namespaceDescription))
			}
		}
	}
	if !changed {
		return body, nil, false
	}

	payload["tools"] = flattened
	rewriteNamespacedCallsForUpstream(payload["input"], mappings)
	rewriteNamespacedToolChoice(payload, mappings)

	normalized, err := json.Marshal(payload)
	if err != nil {
		return body, nil, false
	}
	return normalized, mappings, true
}

// promoteDiscoveredTools 将 xAI 不支持的工具发现输入项转换为顶层工具定义。
// additional_tools 和 tool_search_output 携带的工具会被提升；tool_search_call 仅是发现过程记录，可安全移除。
func promoteDiscoveredTools(payload map[string]any) bool {
	input, ok := payload["input"].([]any)
	if !ok {
		return false
	}
	topLevelTools, _ := payload["tools"].([]any)
	filteredInput := make([]any, 0, len(input))
	changed := false

	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			filteredInput = append(filteredInput, rawItem)
			continue
		}
		itemType, _ := item["type"].(string)
		switch itemType {
		case "additional_tools", "tool_search_output":
			if tools, ok := item["tools"].([]any); ok {
				topLevelTools = append(topLevelTools, tools...)
			}
			changed = true
		case "tool_search_call":
			changed = true
		default:
			filteredInput = append(filteredInput, rawItem)
		}
	}
	if !changed {
		return false
	}
	payload["input"] = filteredInput
	if len(topLevelTools) > 0 {
		payload["tools"] = topLevelTools
	}
	return true
}

const fallbackToolDescription = "Invoke this tool."

func ensureFunctionDescription(tool map[string]any, namespaceDescription string) bool {
	description := namespacedToolDescription(namespaceDescription, tool["description"])
	if description == "" {
		description = fallbackToolDescription
	}
	if tool["description"] == description {
		return false
	}
	tool["description"] = description
	return true
}

func namespacedFunctionTool(child map[string]any, flatName, namespaceDescription string) map[string]any {
	flatTool := map[string]any{"type": "function", "name": flatName}
	description := namespacedToolDescription(namespaceDescription, child["description"])
	if description == "" {
		description = fallbackToolDescription
	}
	flatTool["description"] = description
	for _, key := range []string{"parameters", "strict"} {
		if value, exists := child[key]; exists && value != nil {
			flatTool[key] = value
		}
	}
	return flatTool
}

func customToolAsFunction(tool map[string]any, flatName, namespaceDescription string) map[string]any {
	description := namespacedToolDescription(namespaceDescription, tool["description"])
	if description == "" {
		description = fallbackToolDescription
	}
	return map[string]any{
		"type":        "function",
		"name":        flatName,
		"description": description,
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"input": map[string]any{"type": "string"},
			},
			"required":             []any{"input"},
			"additionalProperties": false,
		},
	}
}

func namespacedToolDescription(namespaceDescription string, childDescription any) string {
	child, _ := childDescription.(string)
	switch {
	case namespaceDescription != "" && child != "":
		return namespaceDescription + "\n" + child
	case namespaceDescription != "":
		return namespaceDescription
	default:
		return child
	}
}

func uniqueNamespaceToolName(namespace, child string, used map[string]bool) string {
	qualifiedName := child
	if namespace != "" {
		qualifiedName = namespace + "__" + child
	}
	base := sanitizeToolName(qualifiedName)
	if base == "" {
		base = "namespace_tool"
	}
	const maxNameBytes = 64
	if len(base) > maxNameBytes {
		sum := sha256.Sum256([]byte(base))
		suffix := "_" + hex.EncodeToString(sum[:4])
		base = base[:maxNameBytes-len(suffix)] + suffix
	}
	if !used[base] {
		return base
	}
	for index := 2; ; index++ {
		suffix := "_" + decimalString(index)
		candidate := base
		if len(candidate)+len(suffix) > maxNameBytes {
			candidate = candidate[:maxNameBytes-len(suffix)]
		}
		candidate += suffix
		if !used[candidate] {
			return candidate
		}
	}
}

func sanitizeToolName(value string) string {
	var out strings.Builder
	for _, r := range value {
		if r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-') {
			out.WriteRune(r)
		} else {
			out.WriteByte('_')
		}
	}
	return strings.Trim(out.String(), "_")
}

func decimalString(value int) string {
	if value == 0 {
		return "0"
	}
	var buf [20]byte
	index := len(buf)
	for value > 0 {
		index--
		buf[index] = byte('0' + value%10)
		value /= 10
	}
	return string(buf[index:])
}

func rewriteNamespacedCallsForUpstream(value any, mappings namespaceToolMappings) {
	reverse := map[string]string{}
	for flatName, mapping := range mappings {
		reverse[mapping.Namespace+"\x00"+mapping.Name] = flatName
	}
	walkJSON(value, func(object map[string]any) {
		switch object["type"] {
		case "function_call":
			namespace, _ := object["namespace"].(string)
			name, _ := object["name"].(string)
			if flatName, ok := reverse[namespace+"\x00"+name]; ok {
				object["name"] = flatName
				delete(object, "namespace")
			}
		case "custom_tool_call":
			namespace, _ := object["namespace"].(string)
			name, _ := object["name"].(string)
			flatName, ok := reverse[namespace+"\x00"+name]
			if !ok || !mappings[flatName].Custom {
				return
			}
			arguments, _ := json.Marshal(map[string]any{"input": customToolInput(object["input"])})
			object["type"] = "function_call"
			object["name"] = flatName
			object["arguments"] = string(arguments)
			delete(object, "input")
			delete(object, "namespace")
		case "custom_tool_call_output":
			object["type"] = "function_call_output"
		}
	})
}

func customToolInput(value any) string {
	if input, ok := value.(string); ok {
		return input
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// normalizeToolChoiceWithoutTools 修复 SDK 在没有工具时仍发送 auto/none tool_choice 的情况。
// required 或指定函数无法在没有工具定义时满足，由调用方返回明确的本地错误。
func normalizeToolChoiceWithoutTools(body []byte) (normalized []byte, changed, invalid bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var payload map[string]any
	if err := dec.Decode(&payload); err != nil {
		return body, false, false
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return body, false, false
	}

	choice, hasChoice := payload["tool_choice"]
	tools, hasToolArray := payload["tools"].([]any)
	hasTools := false
	for _, rawTool := range tools {
		if tool, ok := rawTool.(map[string]any); ok && len(tool) > 0 {
			hasTools = true
			break
		}
	}
	if hasTools {
		return body, false, false
	}
	if hasChoice && !optionalToolChoice(choice) {
		return body, false, true
	}

	if hasChoice {
		delete(payload, "tool_choice")
		changed = true
	}
	if hasToolArray && len(tools) == 0 {
		delete(payload, "tools")
		changed = true
	}
	if !changed {
		return body, false, false
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body, false, false
	}
	return encoded, true, false
}

func optionalToolChoice(choice any) bool {
	if choice == nil {
		return true
	}
	var choiceType string
	switch choice := choice.(type) {
	case string:
		choiceType = choice
	case map[string]any:
		choiceType, _ = choice["type"].(string)
	}
	choiceType = strings.ToLower(strings.TrimSpace(choiceType))
	return choiceType == "auto" || choiceType == "none"
}

func rewriteNamespacedToolChoice(payload map[string]any, mappings namespaceToolMappings) {
	choice, ok := payload["tool_choice"].(map[string]any)
	if !ok {
		return
	}
	namespace, _ := choice["namespace"].(string)
	name, _ := choice["name"].(string)
	for flatName, mapping := range mappings {
		if mapping.Namespace == namespace && mapping.Name == name {
			choice["type"] = "function"
			choice["name"] = flatName
			delete(choice, "namespace")
			return
		}
	}
}

func rewriteNamespaceToolCalls(body []byte, mappings namespaceToolMappings) ([]byte, bool) {
	if len(mappings) == 0 {
		return body, false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var payload any
	if err := dec.Decode(&payload); err != nil {
		return body, false
	}
	changed := false
	walkJSON(payload, func(object map[string]any) {
		if object["type"] != "function_call" {
			return
		}
		name, _ := object["name"].(string)
		mapping, ok := mappings[name]
		if !ok {
			return
		}
		object["name"] = mapping.Name
		if mapping.Namespace != "" {
			object["namespace"] = mapping.Namespace
		}
		if mapping.Custom {
			object["type"] = "custom_tool_call"
			object["input"] = customInputFromArguments(object["arguments"])
			delete(object, "arguments")
		}
		changed = true
	})
	if !changed {
		return body, false
	}
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return rewritten, true
}

func customInputFromArguments(value any) string {
	arguments, ok := value.(string)
	if !ok {
		return customToolInput(value)
	}
	dec := json.NewDecoder(strings.NewReader(arguments))
	dec.UseNumber()
	var object map[string]any
	if err := dec.Decode(&object); err == nil {
		if input, ok := object["input"].(string); ok {
			return input
		}
	}
	return arguments
}

func walkJSON(value any, visit func(map[string]any)) {
	switch value := value.(type) {
	case map[string]any:
		visit(value)
		for _, child := range value {
			walkJSON(child, visit)
		}
	case []any:
		for _, child := range value {
			walkJSON(child, visit)
		}
	}
}

type customToolStreamState struct {
	mappingsByItem  map[string]namespaceToolMapping
	argumentsByItem map[string]string
	inputDoneByItem map[string]bool
}

func (s *customToolStreamState) rewrite(payload []byte, mappings namespaceToolMappings) ([][]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var event map[string]any
	if err := dec.Decode(&event); err != nil {
		return nil, false
	}
	eventType, _ := event["type"].(string)
	itemID, _ := event["item_id"].(string)
	if s.mappingsByItem == nil {
		s.mappingsByItem = make(map[string]namespaceToolMapping)
		s.argumentsByItem = make(map[string]string)
		s.inputDoneByItem = make(map[string]bool)
	}

	switch eventType {
	case "response.output_item.added", "response.output_item.done":
		item, _ := event["item"].(map[string]any)
		if item == nil || item["type"] != "function_call" {
			return nil, false
		}
		name, _ := item["name"].(string)
		mapping, ok := mappings[name]
		if !ok || !mapping.Custom {
			return nil, false
		}
		itemID, _ = item["id"].(string)
		if eventType == "response.output_item.added" && itemID != "" {
			s.mappingsByItem[itemID] = mapping
		}
		item["type"] = "custom_tool_call"
		item["name"] = mapping.Name
		if mapping.Namespace != "" {
			item["namespace"] = mapping.Namespace
		}
		arguments := item["arguments"]
		if customToolInput(arguments) == "" && itemID != "" && s.argumentsByItem[itemID] != "" {
			arguments = s.argumentsByItem[itemID]
		}
		input := customInputFromArguments(arguments)
		item["input"] = input
		delete(item, "arguments")
		payloads := make([][]byte, 0, 2)
		if eventType == "response.output_item.done" && itemID != "" && !s.inputDoneByItem[itemID] && input != "" {
			deltaEvent := map[string]any{
				"type": "response.custom_tool_call_input.delta", "item_id": itemID, "delta": input,
			}
			if outputIndex, exists := event["output_index"]; exists {
				deltaEvent["output_index"] = outputIndex
			}
			deltaPayload, _ := json.Marshal(deltaEvent)
			payloads = append(payloads, deltaPayload)
		}
		encoded, _ := json.Marshal(event)
		payloads = append(payloads, encoded)
		if eventType == "response.output_item.done" && itemID != "" {
			delete(s.mappingsByItem, itemID)
			delete(s.argumentsByItem, itemID)
			delete(s.inputDoneByItem, itemID)
		}
		return payloads, true
	case "response.function_call_arguments.delta":
		mapping, ok := s.mappingsByItem[itemID]
		if !ok || !mapping.Custom {
			return nil, false
		}
		fragment, _ := event["delta"].(string)
		if fragment == "" {
			fragment, _ = event["arguments"].(string)
		}
		s.argumentsByItem[itemID] += fragment
		return [][]byte{}, true
	case "response.function_call_arguments.done":
		mapping, ok := s.mappingsByItem[itemID]
		if !ok || !mapping.Custom {
			return nil, false
		}
		arguments, _ := event["arguments"].(string)
		if arguments == "" {
			arguments = s.argumentsByItem[itemID]
		}
		input := customInputFromArguments(arguments)
		deltaEvent := cloneSchemaValue(event).(map[string]any)
		deltaEvent["type"] = "response.custom_tool_call_input.delta"
		deltaEvent["delta"] = input
		delete(deltaEvent, "arguments")
		doneEvent := cloneSchemaValue(event).(map[string]any)
		doneEvent["type"] = "response.custom_tool_call_input.done"
		doneEvent["input"] = input
		delete(doneEvent, "arguments")
		delete(doneEvent, "delta")
		delete(s.argumentsByItem, itemID)
		s.inputDoneByItem[itemID] = true
		deltaPayload, _ := json.Marshal(deltaEvent)
		donePayload, _ := json.Marshal(doneEvent)
		return [][]byte{deltaPayload, donePayload}, true
	}
	return nil, false
}

func formatSSEPayloads(payloads [][]byte, lineEnding string) string {
	if len(payloads) == 0 {
		return ""
	}
	if lineEnding == "" {
		lineEnding = "\n"
	}
	var output strings.Builder
	for index, payload := range payloads {
		if index > 0 {
			output.WriteString(lineEnding)
		}
		output.WriteString("data: ")
		output.Write(payload)
		output.WriteString(lineEnding)
	}
	return output.String()
}

type anthropicMergedToolStreamState struct {
	contentBlock map[string]any
	originalID   string
	arguments    string
	splitCount   int
}

// splitMergedCalls repairs xAI streams that emit multiple complete tool inputs as
// deltas of one Anthropic tool_use block. Each input must be a separate content block.
func (s *anthropicMergedToolStreamState) splitMergedCalls(payload []byte) ([][]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var event map[string]any
	if err := dec.Decode(&event); err != nil {
		return [][]byte{payload}, false
	}

	eventType, _ := event["type"].(string)
	switch eventType {
	case "content_block_start":
		block, _ := event["content_block"].(map[string]any)
		if block == nil || block["type"] != "tool_use" {
			s.contentBlock = nil
			return [][]byte{payload}, false
		}
		s.contentBlock = cloneSchemaValue(block).(map[string]any)
		s.originalID, _ = block["id"].(string)
		s.arguments = ""
		s.splitCount = 0
	case "content_block_delta":
		if s.contentBlock == nil {
			return [][]byte{payload}, false
		}
		delta, _ := event["delta"].(map[string]any)
		if delta == nil || delta["type"] != "input_json_delta" {
			return [][]byte{payload}, false
		}
		fragment, _ := delta["partial_json"].(string)
		if !completeJSONObject(s.arguments) || !strings.HasPrefix(strings.TrimSpace(fragment), "{") {
			s.arguments += fragment
			return [][]byte{payload}, false
		}

		s.splitCount++
		stopEvent := map[string]any{"type": "content_block_stop"}
		block := cloneSchemaValue(s.contentBlock).(map[string]any)
		block["id"] = fmt.Sprintf("%s-split-%d", s.originalID, s.splitCount)
		block["input"] = map[string]any{}
		startEvent := map[string]any{"type": "content_block_start", "content_block": block}
		deltaEvent := cloneSchemaValue(event).(map[string]any)
		delete(deltaEvent, "index")
		s.arguments = fragment
		s.contentBlock = block

		stopPayload, _ := json.Marshal(stopEvent)
		startPayload, _ := json.Marshal(startEvent)
		deltaPayload, _ := json.Marshal(deltaEvent)
		return [][]byte{stopPayload, startPayload, deltaPayload}, true
	case "content_block_stop":
		s.contentBlock = nil
		s.originalID = ""
		s.arguments = ""
		s.splitCount = 0
	}
	return [][]byte{payload}, false
}

func completeJSONObject(value string) bool {
	var object map[string]any
	return strings.TrimSpace(value) != "" && json.Unmarshal([]byte(value), &object) == nil
}

func formatAnthropicSSEPayloads(payloads [][]byte, lineEnding string) string {
	if lineEnding == "" {
		lineEnding = "\n"
	}
	var output strings.Builder
	for _, payload := range payloads {
		var event struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(payload, &event)
		if event.Type != "" {
			output.WriteString("event: ")
			output.WriteString(event.Type)
			output.WriteString(lineEnding)
		}
		output.WriteString("data: ")
		output.Write(payload)
		output.WriteString(lineEnding)
		output.WriteString(lineEnding)
	}
	return output.String()
}

type streamCompatibilityOptions struct {
	fillAnthropicIndexes    bool
	normalizeAnthropicUsage bool
}

func normalizeXAIAnthropicUsage(data []byte) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var payload any
	if err := dec.Decode(&payload); err != nil || !normalizeAnthropicUsageValue(payload) {
		return data, false
	}
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return data, false
	}
	return rewritten, true
}

func normalizeAnthropicUsageValue(value any) bool {
	changed := false
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "usage" {
				if usage, ok := child.(map[string]any); ok && normalizeAnthropicUsageObject(usage) {
					changed = true
				}
				continue
			}
			if normalizeAnthropicUsageValue(child) {
				changed = true
			}
		}
	case []any:
		for _, child := range value {
			if normalizeAnthropicUsageValue(child) {
				changed = true
			}
		}
	}
	return changed
}

func normalizeAnthropicUsageObject(usage map[string]any) bool {
	input, inputSeen := firstTokenCountPresent(usage, "input_tokens")
	if !inputSeen {
		return false
	}
	cacheRead := firstTokenCount(usage, "cache_read_input_tokens")
	cacheCreation := firstTokenCount(usage, "cache_creation_input_tokens")
	uncachedInput := maxInt(0, input-cacheRead-cacheCreation)
	if uncachedInput == input {
		return false
	}
	usage["input_tokens"] = uncachedInput
	return true
}

func streamCopyWithNamespaceMappings(
	w http.ResponseWriter,
	body io.Reader,
	contentType string,
	mappings namespaceToolMappings,
	start time.Time,
) responseMetrics {
	return streamCopyWithCompatibility(w, body, contentType, mappings, start, streamCompatibilityOptions{})
}

func streamCopyWithCompatibility(
	w http.ResponseWriter,
	body io.Reader,
	contentType string,
	mappings namespaceToolMappings,
	start time.Time,
	options streamCompatibilityOptions,
) responseMetrics {
	if isEventStream(contentType) {
		return streamCopyNamespaceSSE(w, body, mappings, start, options)
	}

	metrics := responseMetrics{}
	data := readAllWithMetrics(body, &metrics, start)
	observeResponsePayload(data, &metrics, start)
	if options.normalizeAnthropicUsage {
		if rewritten, changed := normalizeXAIAnthropicUsage(data); changed {
			data = rewritten
		}
	}
	if rewritten, changed := rewriteNamespaceToolCalls(data, mappings); changed {
		data = rewritten
	}
	_, _ = w.Write(data)
	metrics.finalizeTTFT()
	return metrics
}

func streamCopyNamespaceSSE(
	w http.ResponseWriter,
	body io.Reader,
	mappings namespaceToolMappings,
	start time.Time,
	options streamCompatibilityOptions,
) (metrics responseMetrics) {
	defer metrics.finalizeTTFT()
	reader := bufio.NewReaderSize(body, 32*1024)
	flusher, canFlush := w.(http.Flusher)
	indexState := anthropicStreamIndexState{}
	anthropicToolState := anthropicMergedToolStreamState{}
	customState := customToolStreamState{}
	downstreamWritable := true
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			metrics.markFirstBody(start)
		}
		output := line
		trimmed := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if strings.HasPrefix(trimmed, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if data == "[DONE]" {
				metrics.StreamCompleted = true
			} else if data != "" {
				payload := []byte(data)
				observeResponsePayload(payload, &metrics, start)
				lineEnding := ""
				if strings.HasSuffix(line, "\r\n") {
					lineEnding = "\r\n"
				} else if strings.HasSuffix(line, "\n") {
					lineEnding = "\n"
				}
				if customPayloads, handled := customState.rewrite(payload, mappings); handled {
					output = formatSSEPayloads(customPayloads, lineEnding)
					payload = nil
				}
				if payload != nil {
					payloads := [][]byte{payload}
					expanded := false
					if options.fillAnthropicIndexes {
						payloads, expanded = anthropicToolState.splitMergedCalls(payload)
					}
					changed := false
					for index, current := range payloads {
						if options.fillAnthropicIndexes {
							if rewritten, indexChanged := indexState.fillMissingIndex(current); indexChanged {
								current = rewritten
								changed = true
							}
						}
						if options.normalizeAnthropicUsage {
							if rewritten, usageChanged := normalizeXAIAnthropicUsage(current); usageChanged {
								current = rewritten
								changed = true
							}
						}
						if rewritten, namespaceChanged := rewriteNamespaceToolCalls(current, mappings); namespaceChanged {
							current = rewritten
							changed = true
						}
						payloads[index] = current
					}
					if expanded {
						output = formatAnthropicSSEPayloads(payloads, lineEnding)
					} else if changed {
						output = "data: " + string(payloads[0]) + lineEnding
					}
				}
			}
		}
		if downstreamWritable && output != "" {
			if _, writeErr := io.WriteString(w, output); writeErr != nil {
				downstreamWritable = false
				metrics.DownstreamDisconnected = true
			} else if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			metrics.UpstreamReadError = !errors.Is(err, io.EOF)
			return metrics
		}
	}
}

type anthropicStreamIndexState struct {
	activeIndex int
	hasActive   bool
	nextIndex   int
}

func (s *anthropicStreamIndexState) fillMissingIndex(data []byte) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var payload map[string]any
	if err := dec.Decode(&payload); err != nil {
		return data, false
	}

	eventType, _ := payload["type"].(string)
	index, hasIndex := anthropicEventIndex(payload)
	changed := false
	switch eventType {
	case "content_block_start":
		if !hasIndex || index < s.nextIndex {
			index = s.nextIndex
			payload["index"] = index
			changed = true
		}
		s.setActive(index)
	case "content_block_delta":
		if s.hasActive {
			if !hasIndex || index != s.activeIndex {
				payload["index"] = s.activeIndex
				changed = true
			}
		} else if hasIndex {
			s.setActive(index)
		}
	case "content_block_stop":
		if s.hasActive && (!hasIndex || index != s.activeIndex) {
			payload["index"] = s.activeIndex
			changed = true
		}
		s.hasActive = false
	default:
		return data, false
	}
	if !changed {
		return data, false
	}
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return data, false
	}
	return rewritten, true
}

func anthropicEventIndex(payload map[string]any) (int, bool) {
	value, exists := payload["index"]
	if !exists {
		return 0, false
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	index, err := number.Int64()
	return int(index), err == nil && index >= 0
}

func (s *anthropicStreamIndexState) setActive(index int) {
	s.activeIndex = index
	s.hasActive = true
	if index >= s.nextIndex {
		s.nextIndex = index + 1
	}
}

func streamCopyRaw(w http.ResponseWriter, body io.Reader) {
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}
