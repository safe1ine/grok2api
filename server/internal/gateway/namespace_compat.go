package gateway

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
)

type namespaceToolMapping struct {
	Namespace string
	Name      string
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
		if tool, ok := rawTool.(map[string]any); ok && tool["type"] == "function" {
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
		if !ok || tool["type"] != "namespace" {
			flattened = append(flattened, rawTool)
			continue
		}
		changed = true
		namespace, _ := tool["name"].(string)
		namespaceDescription, _ := tool["description"].(string)
		children, _ := tool["tools"].([]any)
		for _, rawChild := range children {
			child, ok := rawChild.(map[string]any)
			if !ok || child["type"] != "function" {
				continue // xAI 不支持 namespace 中的 custom 工具。
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
			mappings[flatName] = namespaceToolMapping{Namespace: namespace, Name: childName}

			flatTool := map[string]any{"type": "function", "name": flatName}
			if description := namespacedToolDescription(namespaceDescription, child["description"]); description != "" {
				flatTool["description"] = description
			}
			for _, key := range []string{"parameters", "strict"} {
				if value, exists := child[key]; exists && value != nil {
					flatTool[key] = value
				}
			}
			flattened = append(flattened, flatTool)
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
	base := sanitizeToolName(namespace + "__" + child)
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
		if object["type"] != "function_call" {
			return
		}
		namespace, _ := object["namespace"].(string)
		name, _ := object["name"].(string)
		if flatName, ok := reverse[namespace+"\x00"+name]; ok {
			object["name"] = flatName
			delete(object, "namespace")
		}
	})
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
		object["namespace"] = mapping.Namespace
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

type streamCompatibilityOptions struct {
	fillAnthropicIndexes bool
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
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			metrics.markFirstBody(start)
		}
		output := line
		trimmed := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if strings.HasPrefix(trimmed, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if data != "" && data != "[DONE]" {
				payload := []byte(data)
				observeResponsePayload(payload, &metrics, start)
				changed := false
				if options.fillAnthropicIndexes {
					if rewritten, indexChanged := indexState.fillMissingIndex(payload); indexChanged {
						payload = rewritten
						changed = true
					}
				}
				if rewritten, namespaceChanged := rewriteNamespaceToolCalls(payload, mappings); namespaceChanged {
					payload = rewritten
					changed = true
				}
				if changed {
					lineEnding := ""
					if strings.HasSuffix(line, "\r\n") {
						lineEnding = "\r\n"
					} else if strings.HasSuffix(line, "\n") {
						lineEnding = "\n"
					}
					output = "data: " + string(payload) + lineEnding
				}
			}
		}
		if output != "" {
			if _, writeErr := io.WriteString(w, output); writeErr != nil {
				return metrics
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
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
		if !hasIndex {
			index = s.nextIndex
			payload["index"] = index
			changed = true
		}
		s.setActive(index)
	case "content_block_delta":
		if hasIndex {
			s.setActive(index)
		} else if s.hasActive {
			payload["index"] = s.activeIndex
			changed = true
		}
	case "content_block_stop":
		if !hasIndex && s.hasActive {
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
