package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// normalizeToolSchemas 修正下游 SDK 生成、但 xAI 严格校验不接受的工具 JSON Schema。
// 未发生修改时返回原始 body，避免普通透传请求被重新编码。
func normalizeToolSchemas(body []byte) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()

	var payload map[string]any
	if err := dec.Decode(&payload); err != nil {
		return body, false
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return body, false
	}

	// required:null 在任何 JSON Schema 位置都不合法。先全请求递归改为空数组，
	// 覆盖 output_schema、text.format.schema、output_config 等非 tools 容器。
	changed := removeNullRequired(payload)

	tools, _ := payload["tools"].([]any)
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}

		// Anthropic Messages API: tools[].input_schema
		if schema, ok := tool["input_schema"].(map[string]any); ok {
			changed = normalizeSchema(schema, true) || changed
		}

		// OpenAI Responses API: tools[].parameters
		if schema, ok := tool["parameters"].(map[string]any); ok {
			changed = normalizeSchema(schema, true) || changed
		}

		// OpenAI Chat Completions API: tools[].function.parameters
		if function, ok := tool["function"].(map[string]any); ok {
			if schema, ok := function["parameters"].(map[string]any); ok {
				changed = normalizeSchema(schema, true) || changed
			}
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

// normalizeSchema 递归修正规范问题。root 表示该 schema 是函数参数根节点，
// xAI 要求该节点最终只能接受 JSON object。
func normalizeSchema(schema map[string]any, root bool) bool {
	changed := false
	if required, exists := schema["required"]; exists && required == nil {
		schema["required"] = []any{}
		changed = true
	}

	for _, value := range schema {
		changed = normalizeSchemaValue(value) || changed
	}
	if root {
		changed = normalizeRootObjectSchema(schema, schema) || changed
	}
	// xAI 的 Anthropic 适配层会把缺失 required 的 object Schema 编码成 null，
	// 随后又因 required 不是数组而拒绝请求，因此显式补空数组。
	changed = ensureObjectRequiredArrays(schema) || changed
	return changed
}

func removeNullRequired(value any) bool {
	changed := false
	switch value := value.(type) {
	case map[string]any:
		if required, exists := value["required"]; exists && required == nil {
			value["required"] = []any{}
			changed = true
		}
		for _, child := range value {
			changed = removeNullRequired(child) || changed
		}
	case []any:
		for _, child := range value {
			changed = removeNullRequired(child) || changed
		}
	}
	return changed
}

func ensureObjectRequiredArrays(value any) bool {
	changed := false
	switch value := value.(type) {
	case map[string]any:
		if schemaAllowsObjectDirectly(value) {
			if required, exists := value["required"]; !exists || required == nil {
				value["required"] = []any{}
				changed = true
			}
		}
		for _, child := range value {
			changed = ensureObjectRequiredArrays(child) || changed
		}
	case []any:
		for _, child := range value {
			changed = ensureObjectRequiredArrays(child) || changed
		}
	}
	return changed
}

func schemaAllowsObjectDirectly(schema map[string]any) bool {
	switch schemaType := schema["type"].(type) {
	case string:
		return schemaType == "object"
	case []any:
		for _, item := range schemaType {
			if item == "object" {
				return true
			}
		}
	}
	_, hasProperties := schema["properties"]
	return hasProperties
}

func normalizeSchemaValue(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		return normalizeSchema(value, false)
	case []any:
		changed := false
		for _, item := range value {
			changed = normalizeSchemaValue(item) || changed
		}
		return changed
	default:
		return false
	}
}

func normalizeRootObjectSchema(schema, root map[string]any) bool {
	changed := narrowObjectType(schema)
	rootIsObject := schemaDirectlyRequiresObject(schema)

	for _, unionKey := range []string{"anyOf", "oneOf"} {
		rawBranches, ok := schema[unionKey].([]any)
		if !ok || len(rawBranches) == 0 {
			continue
		}

		// 当根节点已经约束为 object 时，oneOf/anyOf 的无类型分支只是附加条件。
		// 标准 JSON Schema 会把这些约束与根 type 合并，但 xAI 要求每个分支显式声明 object。
		if rootIsObject {
			objectBranches := make([]any, 0, len(rawBranches))
			for _, rawBranch := range rawBranches {
				branch, ok := rawBranch.(map[string]any)
				if !ok {
					changed = true
					continue
				}
				changed = normalizeRootObjectSchema(branch, root) || changed
				if schemaExplicitlyRejectsObject(branch) {
					changed = true
					continue
				}
				if !schemaAllowsObject(branch, root, map[string]bool{}) {
					branch["type"] = "object"
					changed = true
				}
				objectBranches = append(objectBranches, branch)
			}
			if len(objectBranches) > 0 {
				schema[unionKey] = objectBranches
			} else {
				delete(schema, unionKey)
			}
			continue
		}

		// 根节点自身没有 object 约束时，只保留能够确认接受 object 的联合分支。
		objectBranches := make([]any, 0, len(rawBranches))
		for _, rawBranch := range rawBranches {
			branch, ok := rawBranch.(map[string]any)
			if !ok {
				continue
			}
			changed = normalizeRootObjectSchema(branch, root) || changed
			if schemaAllowsObject(branch, root, map[string]bool{}) {
				objectBranches = append(objectBranches, branch)
			}
		}

		if len(objectBranches) == 0 {
			continue
		}
		if len(objectBranches) != len(rawBranches) {
			changed = true
		}
		if len(objectBranches) == 1 {
			delete(schema, unionKey)
			branch := objectBranches[0].(map[string]any)
			for key, value := range branch {
				if _, exists := schema[key]; !exists {
					schema[key] = value
				}
			}
		} else {
			schema[unionKey] = objectBranches
		}
		schema["type"] = "object"
		rootIsObject = true
	}

	return changed
}

func schemaDirectlyRequiresObject(schema map[string]any) bool {
	if schemaType, ok := schema["type"].(string); ok {
		return schemaType == "object"
	}
	_, hasProperties := schema["properties"]
	return hasProperties
}

func schemaExplicitlyRejectsObject(schema map[string]any) bool {
	switch schemaType := schema["type"].(type) {
	case string:
		return schemaType != "object"
	case []any:
		for _, item := range schemaType {
			if item == "object" {
				return false
			}
		}
		return true
	}
	return false
}

func narrowObjectType(schema map[string]any) bool {
	types, ok := schema["type"].([]any)
	if !ok {
		return false
	}
	for _, schemaType := range types {
		if schemaType == "object" {
			schema["type"] = "object"
			return true
		}
	}
	return false
}

func schemaAllowsObject(schema, root map[string]any, seen map[string]bool) bool {
	switch schemaType := schema["type"].(type) {
	case string:
		return schemaType == "object"
	case []any:
		for _, item := range schemaType {
			if item == "object" {
				return true
			}
		}
	}
	if _, hasProperties := schema["properties"]; hasProperties {
		return true
	}

	if ref, ok := schema["$ref"].(string); ok && !seen[ref] {
		seen[ref] = true
		if target, ok := resolveLocalSchemaRef(root, ref); ok {
			if schemaAllowsObject(target, root, seen) {
				return true
			}
		}
	}

	for _, keyword := range []string{"anyOf", "oneOf", "allOf"} {
		branches, ok := schema[keyword].([]any)
		if !ok {
			continue
		}
		for _, rawBranch := range branches {
			if branch, ok := rawBranch.(map[string]any); ok && schemaAllowsObject(branch, root, seen) {
				return true
			}
		}
	}
	return false
}

// toolSchemaSummary 仅输出工具 Schema 的结构信息，不包含 messages、input、描述或工具实参。
func toolSchemaSummary(body []byte) string {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var payload map[string]any
	if err := dec.Decode(&payload); err != nil {
		return ""
	}
	tools, ok := payload["tools"].([]any)
	if !ok {
		return ""
	}

	summaries := make([]any, 0, len(tools))
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		name, _ := tool["name"].(string)
		var schema map[string]any
		var format string
		if value, ok := tool["parameters"].(map[string]any); ok {
			schema, format = value, "parameters"
		} else if value, ok := tool["input_schema"].(map[string]any); ok {
			schema, format = value, "input_schema"
		} else if function, ok := tool["function"].(map[string]any); ok {
			if name == "" {
				name, _ = function["name"].(string)
			}
			if value, ok := function["parameters"].(map[string]any); ok {
				schema, format = value, "function.parameters"
			}
		}
		if schema == nil {
			continue
		}
		summaries = append(summaries, map[string]any{
			"name":   name,
			"format": format,
			"root":   summarizeSchemaStructure(schema, 0),
		})
	}
	if len(summaries) == 0 {
		return ""
	}
	encoded, err := json.Marshal(summaries)
	if err != nil {
		return ""
	}
	const maxSummaryBytes = 16 * 1024
	if len(encoded) > maxSummaryBytes {
		return string(encoded[:maxSummaryBytes]) + `...(truncated)`
	}
	return string(encoded)
}

func summarizeSchemaStructure(schema map[string]any, depth int) map[string]any {
	if depth >= 8 {
		return map[string]any{"truncated": true}
	}
	out := map[string]any{}
	for _, key := range []string{"type", "$ref", "required", "additionalProperties"} {
		if value, exists := schema[key]; exists {
			switch value := value.(type) {
			case map[string]any:
				out[key] = summarizeSchemaStructure(value, depth+1)
			case []any:
				if key == "required" || key == "type" {
					out[key] = value
				}
			case string, bool, nil:
				out[key] = value
			}
		}
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if branches, ok := schema[key].([]any); ok {
			items := make([]any, 0, len(branches))
			for _, rawBranch := range branches {
				if branch, ok := rawBranch.(map[string]any); ok {
					items = append(items, summarizeSchemaStructure(branch, depth+1))
				} else {
					items = append(items, map[string]any{"json_type": fmt.Sprintf("%T", rawBranch)})
				}
			}
			out[key] = items
		}
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		names := make([]string, 0, len(properties))
		for name := range properties {
			names = append(names, name)
		}
		sort.Strings(names)
		out["property_names"] = names
	}
	keys := make([]string, 0, len(schema))
	for key := range schema {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out["keys"] = keys
	return out
}

func resolveLocalSchemaRef(root map[string]any, ref string) (map[string]any, bool) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	var current any = root
	for _, token := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[token]
		if !ok {
			return nil, false
		}
	}
	target, ok := current.(map[string]any)
	return target, ok
}
