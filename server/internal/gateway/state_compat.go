package gateway

import (
	"bytes"
	"encoding/json"
	"strings"
)

type stateCompatibilityIssue string

const (
	issueEncryptedContent stateCompatibilityIssue = "encrypted_content"
	issueReasoningEffort  stateCompatibilityIssue = "reasoning_effort"
	issueCompactionBlob   stateCompatibilityIssue = "compaction_blob"
)

func unsupportedArgument(body []byte) string {
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		return ""
	}
	message, _ := response["error"].(string)
	const prefix = "argument not supported:"
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(message)), prefix) {
		return ""
	}
	argument := strings.TrimSpace(strings.TrimSpace(message)[len(prefix):])
	argument = strings.TrimSuffix(argument, ".")
	if argument == "" {
		return ""
	}
	for _, r := range argument {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return ""
		}
	}
	return argument
}

func removeUnsupportedArgument(body []byte, argument string) ([]byte, bool) {
	if argument == "" || argument == "model" || argument == "input" || argument == "messages" || argument == "tools" {
		return body, false
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, false
	}

	changed := false
	if _, exists := payload[argument]; exists {
		delete(payload, argument)
		changed = true
	}
	if tools, ok := payload["tools"].([]any); ok {
		for _, rawTool := range tools {
			changed = removeUnsupportedBuiltInToolArgument(rawTool, argument) || changed
		}
	}
	if !changed {
		return body, false
	}
	normalized, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return normalized, true
}

func removeUnsupportedBuiltInToolArgument(value any, argument string) bool {
	tool, ok := value.(map[string]any)
	if !ok {
		return false
	}
	toolType, _ := tool["type"].(string)
	if toolType == "function" {
		return false
	}

	changed := false
	if _, exists := tool[argument]; exists {
		delete(tool, argument)
		changed = true
	}
	if children, ok := tool["tools"].([]any); ok {
		for _, child := range children {
			changed = removeUnsupportedBuiltInToolArgument(child, argument) || changed
		}
	}
	return changed
}

func classifyStateCompatibilityError(body []byte) stateCompatibilityIssue {
	lower := bytes.ToLower(body)
	switch {
	case bytes.Contains(lower, []byte("could not decrypt the provided encrypted_content")):
		return issueEncryptedContent
	case bytes.Contains(lower, []byte("invalid reasoning effort")):
		return issueReasoningEffort
	case bytes.Contains(lower, []byte("could not decode the compaction blob")):
		return issueCompactionBlob
	default:
		return ""
	}
}

// applyStateCompatibilityFallback 只在上游明确拒绝某类状态后调用。
// 它保留普通消息和工具历史，只移除无法继续使用的 opaque 状态。
func applyStateCompatibilityFallback(body []byte, issue stateCompatibilityIssue) ([]byte, bool) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, false
	}

	changed := false
	switch issue {
	case issueReasoningEffort:
		if _, exists := payload["reasoning_effort"]; exists {
			delete(payload, "reasoning_effort")
			changed = true
		}
		if reasoning, ok := payload["reasoning"].(map[string]any); ok {
			if _, exists := reasoning["effort"]; exists {
				delete(reasoning, "effort")
				changed = true
			}
			if len(reasoning) == 0 {
				delete(payload, "reasoning")
			}
		}
	case issueEncryptedContent, issueCompactionBlob:
		if _, exists := payload["previous_response_id"]; exists {
			delete(payload, "previous_response_id")
			changed = true
		}
		cleaned, stateChanged, _ := removeInvalidOpaqueState(payload)
		if root, ok := cleaned.(map[string]any); ok {
			payload = root
		}
		changed = stateChanged || changed
	}
	if !changed {
		return body, false
	}

	normalized, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return normalized, true
}

func removeInvalidOpaqueState(value any) (any, bool, bool) {
	switch value := value.(type) {
	case []any:
		changed := false
		cleaned := make([]any, 0, len(value))
		for _, item := range value {
			next, itemChanged, remove := removeInvalidOpaqueState(item)
			changed = itemChanged || changed
			if remove {
				changed = true
				continue
			}
			cleaned = append(cleaned, next)
		}
		return cleaned, changed, false
	case map[string]any:
		itemType, _ := value["type"].(string)
		itemType = strings.ToLower(itemType)
		_, hasEncryptedContent := value["encrypted_content"]
		if (itemType == "reasoning" && hasEncryptedContent) || isCompactionItemType(itemType) {
			return nil, true, true
		}

		changed := false
		for _, key := range []string{"encrypted_content", "compaction_blob", "compact_blob", "encrypted_compaction"} {
			if _, exists := value[key]; exists {
				delete(value, key)
				changed = true
			}
		}
		for key, child := range value {
			next, childChanged, remove := removeInvalidOpaqueState(child)
			changed = childChanged || changed
			if remove {
				delete(value, key)
				changed = true
				continue
			}
			value[key] = next
		}
		return value, changed, false
	default:
		return value, false, false
	}
}

func isCompactionItemType(itemType string) bool {
	return itemType == "compaction" || itemType == "compact" || itemType == "compacted"
}

// stateCompatibilitySummary 不包含提示词、密文、工具参数或压缩内容。
func stateCompatibilitySummary(body []byte) map[string]any {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}

	summary := map[string]any{}
	if effort, ok := payload["reasoning_effort"].(string); ok {
		summary["reasoning_effort"] = effort
	}
	if reasoning, ok := payload["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok {
			summary["reasoning_effort"] = effort
		}
	}
	if _, exists := payload["previous_response_id"]; exists {
		summary["has_previous_response_id"] = true
	}

	var itemTypes []string
	var encryptedLengths []int
	collectOpaqueStateSummary(payload["input"], &itemTypes, &encryptedLengths)
	if len(itemTypes) > 0 {
		summary["state_item_types"] = itemTypes
	}
	if len(encryptedLengths) > 0 {
		summary["encrypted_lengths"] = encryptedLengths
	}
	if len(summary) == 0 {
		return nil
	}
	return summary
}

func collectOpaqueStateSummary(value any, itemTypes *[]string, encryptedLengths *[]int) {
	switch value := value.(type) {
	case []any:
		for _, child := range value {
			collectOpaqueStateSummary(child, itemTypes, encryptedLengths)
		}
	case map[string]any:
		itemType, _ := value["type"].(string)
		if itemType == "reasoning" || isCompactionItemType(strings.ToLower(itemType)) {
			*itemTypes = append(*itemTypes, itemType)
		}
		if encrypted, ok := value["encrypted_content"].(string); ok {
			*encryptedLengths = append(*encryptedLengths, len(encrypted))
		}
		for _, child := range value {
			collectOpaqueStateSummary(child, itemTypes, encryptedLengths)
		}
	}
}
