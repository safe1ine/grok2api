package gateway

import "encoding/json"

// normalizeAnthropicMessageRoles 修正 OpenAI 风格客户端误发到 Anthropic
// /messages 端点的消息角色。Anthropic messages 只接受 user/assistant，
// system/developer 指令必须位于请求顶层 system 字段。
func normalizeAnthropicMessageRoles(body []byte) ([]byte, bool) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, false
	}

	messages, ok := payload["messages"].([]any)
	if !ok {
		return body, false
	}

	changed := false
	filtered := make([]any, 0, len(messages))
	var promotedSystem []any
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			filtered = append(filtered, rawMessage)
			continue
		}

		role, _ := message["role"].(string)
		switch role {
		case "system", "developer":
			promotedSystem = append(promotedSystem, anthropicSystemBlocks(message["content"])...)
			changed = true
		case "tool", "function":
			message["role"] = "user"
			if toolUseID := firstString(message, "tool_call_id", "tool_use_id"); toolUseID != "" {
				block := map[string]any{
					"type":        "tool_result",
					"tool_use_id": toolUseID,
					"content":     message["content"],
				}
				message["content"] = []any{block}
				delete(message, "tool_call_id")
				delete(message, "tool_use_id")
				delete(message, "name")
			}
			filtered = append(filtered, message)
			changed = true
		case "human":
			message["role"] = "user"
			filtered = append(filtered, message)
			changed = true
		case "model":
			message["role"] = "assistant"
			filtered = append(filtered, message)
			changed = true
		default:
			filtered = append(filtered, message)
		}
	}

	if len(promotedSystem) > 0 {
		existing := anthropicSystemBlocks(payload["system"])
		payload["system"] = append(existing, promotedSystem...)
	}
	if !changed {
		return body, false
	}
	payload["messages"] = filtered

	normalized, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return normalized, true
}

func anthropicSystemBlocks(content any) []any {
	switch content := content.(type) {
	case nil:
		return nil
	case string:
		if content == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": content}}
	case []any:
		blocks := make([]any, 0, len(content))
		for _, item := range content {
			if text, ok := item.(string); ok {
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
				continue
			}
			blocks = append(blocks, item)
		}
		return blocks
	default:
		return []any{content}
	}
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

// messageRoleSummary 只输出消息角色与内容块类型，不输出提示词、工具参数或结果。
func messageRoleSummary(body []byte) any {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	messages, ok := payload["messages"].([]any)
	if !ok {
		return nil
	}

	roles := make([]any, 0, len(messages))
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			roles = append(roles, "non_object")
			continue
		}
		entry := map[string]any{"role": message["role"]}
		if blocks, ok := message["content"].([]any); ok {
			types := make([]any, 0, len(blocks))
			for _, rawBlock := range blocks {
				if block, ok := rawBlock.(map[string]any); ok {
					types = append(types, block["type"])
				}
			}
			entry["content_types"] = types
		}
		roles = append(roles, entry)
	}
	return map[string]any{"has_top_level_system": payload["system"] != nil, "messages": roles}
}
