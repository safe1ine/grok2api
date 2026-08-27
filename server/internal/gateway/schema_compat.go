package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
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
			changed = ensureFunctionDescription(tool, "") || changed
			changed = inlineLocalSchemaRefs(schema) || changed
			changed = normalizeSchema(schema, true) || changed
		}

		// OpenAI Responses API: tools[].parameters
		if schema, ok := tool["parameters"].(map[string]any); ok {
			changed = ensureFunctionDescription(tool, "") || changed
			changed = inlineLocalSchemaRefs(schema) || changed
			changed = normalizeSchema(schema, true) || changed
		}

		// OpenAI Chat Completions API: tools[].function.parameters
		if function, ok := tool["function"].(map[string]any); ok {
			if schema, ok := function["parameters"].(map[string]any); ok {
				changed = ensureFunctionDescription(function, "") || changed
				changed = inlineLocalSchemaRefs(schema) || changed
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

// inlineLocalSchemaRefs 将标准 JSON Schema 的本地引用内联。xAI 会把嵌套 items
// 当成独立 Schema 校验，因而无法从那里解析根级 #/$defs/...。
// relaxToolParameterRoots 仅在上游明确拒绝工具根 Schema 后使用。
// 它保留已知属性定义，但移除根级组合约束和 required，交由实际 MCP 工具做最终参数校验。
func relaxToolParameterRoots(body []byte) ([]byte, bool) {
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

	changed := false
	tools, _ := payload["tools"].([]any)
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		if schema, ok := tool["input_schema"].(map[string]any); ok {
			changed = relaxRootObjectSchema(schema) || changed
		}
		if schema, ok := tool["parameters"].(map[string]any); ok {
			changed = relaxRootObjectSchema(schema) || changed
		}
		if function, ok := tool["function"].(map[string]any); ok {
			if schema, ok := function["parameters"].(map[string]any); ok {
				changed = relaxRootObjectSchema(schema) || changed
			}
		}
	}
	if !changed {
		return body, false
	}
	updated, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return updated, true
}

func relaxRootObjectSchema(schema map[string]any) bool {
	properties, _ := cloneSchemaValue(schema["properties"]).(map[string]any)
	if properties == nil {
		properties = map[string]any{}
	}
	relaxed := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             []any{},
		"additionalProperties": true,
	}
	if reflect.DeepEqual(schema, relaxed) {
		return false
	}
	clear(schema)
	for key, value := range relaxed {
		schema[key] = value
	}
	return true
}

func inlineLocalSchemaRefs(root map[string]any) bool {
	lookup, ok := cloneSchemaValue(root).(map[string]any)
	if !ok {
		return false
	}
	changed := inlineLocalSchemaRefsValue(root, lookup, map[string]bool{}, 0)
	if !containsLocalSchemaRef(root) {
		if _, exists := root["$defs"]; exists {
			delete(root, "$defs")
			changed = true
		}
		if _, exists := root["definitions"]; exists {
			delete(root, "definitions")
			changed = true
		}
	}
	return changed
}

func inlineLocalSchemaRefsValue(value any, lookup map[string]any, resolving map[string]bool, depth int) bool {
	if depth >= 64 {
		return false
	}
	changed := false
	switch value := value.(type) {
	case map[string]any:
		if ref, ok := value["$ref"].(string); ok && strings.HasPrefix(ref, "#/") && !resolving[ref] {
			if target, ok := resolveLocalSchemaRef(lookup, ref); ok {
				resolved, _ := cloneSchemaValue(target).(map[string]any)
				resolving[ref] = true
				changed = inlineLocalSchemaRefsValue(resolved, lookup, resolving, depth+1) || changed
				delete(resolving, ref)

				siblings := make(map[string]any, len(value)-1)
				for key, child := range value {
					if key != "$ref" {
						siblings[key] = child
					}
				}
				for _, child := range siblings {
					changed = inlineLocalSchemaRefsValue(child, lookup, resolving, depth+1) || changed
				}
				clear(value)
				for key, child := range resolved {
					value[key] = child
				}
				for key, child := range siblings {
					value[key] = child
				}
				return true
			}
		}
		for _, child := range value {
			changed = inlineLocalSchemaRefsValue(child, lookup, resolving, depth+1) || changed
		}
	case []any:
		for _, child := range value {
			changed = inlineLocalSchemaRefsValue(child, lookup, resolving, depth+1) || changed
		}
	}
	return changed
}

func cloneSchemaValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		clone := make(map[string]any, len(value))
		for key, child := range value {
			clone[key] = cloneSchemaValue(child)
		}
		return clone
	case []any:
		clone := make([]any, len(value))
		for index, child := range value {
			clone[index] = cloneSchemaValue(child)
		}
		return clone
	default:
		return value
	}
}

func containsLocalSchemaRef(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		if ref, ok := value["$ref"].(string); ok && strings.HasPrefix(ref, "#/") {
			return true
		}
		for _, child := range value {
			if containsLocalSchemaRef(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if containsLocalSchemaRef(child) {
				return true
			}
		}
	}
	return false
}

// normalizeSchema 递归修正规范问题。root 表示该 schema 是函数参数根节点，
// xAI 要求该节点最终只能接受 JSON object。
func normalizeSchema(schema map[string]any, root bool) bool {
	changed := false
	if required, exists := schema["required"]; exists && required == nil {
		schema["required"] = []any{}
		changed = true
	}
	changed = normalizeChildSchemas(schema) || changed
	if root {
		changed = normalizeRootObjectSchema(schema, schema) || changed
	}
	// xAI 的 Anthropic 适配层会把缺失 required 的 object Schema 编码成 null，
	// 随后又因 required 不是数组而拒绝请求，因此显式补空数组。
	changed = ensureObjectRequiredArrays(schema) || changed
	return changed
}

func normalizeChildSchemas(schema map[string]any) bool {
	changed := false
	for _, key := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"} {
		container, ok := schema[key].(map[string]any)
		if !ok {
			continue
		}
		for name, rawChild := range container {
			switch child := rawChild.(type) {
			case map[string]any:
				changed = normalizeSchema(child, false) || changed
			case bool:
				// JSON Schema 允许 boolean schema。
			default:
				// properties 下的 null/数组不是合法 Schema；空 object 是最宽松的安全降级。
				container[name] = map[string]any{}
				changed = true
			}
		}
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf", "prefixItems"} {
		children, ok := schema[key].([]any)
		if !ok {
			continue
		}
		for index, rawChild := range children {
			switch child := rawChild.(type) {
			case map[string]any:
				changed = normalizeSchema(child, false) || changed
			case bool:
			default:
				children[index] = map[string]any{}
				changed = true
			}
		}
	}
	for _, key := range []string{
		"items", "additionalProperties", "unevaluatedProperties", "contains", "propertyNames",
		"not", "if", "then", "else",
	} {
		rawChild, exists := schema[key]
		if !exists {
			continue
		}
		switch child := rawChild.(type) {
		case map[string]any:
			changed = normalizeSchema(child, false) || changed
		case []any:
			for index, rawItem := range child {
				if item, ok := rawItem.(map[string]any); ok {
					changed = normalizeSchema(item, false) || changed
				} else if _, ok := rawItem.(bool); !ok {
					child[index] = map[string]any{}
					changed = true
				}
			}
		case bool:
		default:
			schema[key] = map[string]any{}
			changed = true
		}
	}
	return changed
}

func removeNullRequired(value any) bool {
	return removeNullRequiredValue(value, false)
}

func removeNullRequiredValue(value any, namedSchemaContainer bool) bool {
	changed := false
	switch value := value.(type) {
	case map[string]any:
		if namedSchemaContainer {
			for _, child := range value {
				changed = removeNullRequiredValue(child, false) || changed
			}
			return changed
		}
		if required, exists := value["required"]; exists && required == nil {
			value["required"] = []any{}
			changed = true
		}
		for key, child := range value {
			container := key == "properties" || key == "patternProperties" || key == "$defs" ||
				key == "definitions" || key == "dependentSchemas"
			changed = removeNullRequiredValue(child, container) || changed
		}
	case []any:
		for _, child := range value {
			changed = removeNullRequiredValue(child, false) || changed
		}
	}
	return changed
}

func ensureObjectRequiredArrays(value any) bool {
	schema, ok := value.(map[string]any)
	if !ok || !schemaAllowsObjectDirectly(schema) {
		return false
	}
	if required, exists := schema["required"]; !exists || required == nil {
		schema["required"] = []any{}
		return true
	}
	return false
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

func normalizeRootObjectSchema(schema, root map[string]any) bool {
	changed := narrowObjectType(schema)
	changed = mergeRootAllOf(schema, root) || changed
	rootIsObject := schemaDirectlyRequiresObject(schema)

	for _, unionKey := range []string{"anyOf", "oneOf"} {
		rawBranches, ok := schema[unionKey].([]any)
		if !ok || len(rawBranches) == 0 {
			continue
		}

		objectBranches := make([]map[string]any, 0, len(rawBranches))
		for _, rawBranch := range rawBranches {
			branch, ok := rawBranch.(map[string]any)
			if !ok {
				changed = true
				continue
			}
			changed = normalizeRootObjectSchema(branch, root) || changed
			acceptsObject := !schemaExplicitlyRejectsObject(branch)
			if !rootIsObject {
				acceptsObject = schemaAllowsObject(branch, root, map[string]bool{})
			}
			if !acceptsObject {
				changed = true
				continue
			}
			if branch["type"] != "object" {
				branch["type"] = "object"
				changed = true
			}
			objectBranches = append(objectBranches, branch)
		}

		delete(schema, unionKey)
		schema["type"] = "object"
		rootIsObject = true
		changed = true
		if len(objectBranches) == 0 {
			// 纯标量联合无法保留为工具参数，降级成宽松 object。
			if _, exists := schema["properties"]; !exists {
				schema["properties"] = map[string]any{}
			}
			schema["additionalProperties"] = true
			continue
		}
		changed = mergeAlternativeObjectBranches(schema, objectBranches) || changed
	}

	return changed
}

func mergeRootAllOf(schema, root map[string]any) bool {
	rawBranches, ok := schema["allOf"].([]any)
	if !ok || len(rawBranches) == 0 {
		return false
	}
	changed := false
	required := requiredSet(schema)
	allAdditionalPropertiesFalse := true
	retained := 0
	for _, rawBranch := range rawBranches {
		branch, ok := rawBranch.(map[string]any)
		if !ok {
			continue
		}
		changed = normalizeRootObjectSchema(branch, root) || changed
		if schemaExplicitlyRejectsObject(branch) {
			continue
		}
		retained++
		changed = mergeObjectProperties(schema, branch, "allOf") || changed
		for name := range requiredSet(branch) {
			required[name] = true
		}
		if branch["additionalProperties"] != false {
			allAdditionalPropertiesFalse = false
		}
	}
	delete(schema, "allOf")
	schema["type"] = "object"
	setRequired(schema, required)
	if retained > 0 && allAdditionalPropertiesFalse {
		if _, exists := schema["additionalProperties"]; !exists {
			schema["additionalProperties"] = false
		}
	}
	return true
}

func mergeAlternativeObjectBranches(schema map[string]any, branches []map[string]any) bool {
	changed := false
	var commonRequired map[string]bool
	allAdditionalPropertiesFalse := true
	for _, branch := range branches {
		changed = mergeObjectProperties(schema, branch, "anyOf") || changed
		branchRequired := requiredSet(branch)
		if commonRequired == nil {
			commonRequired = branchRequired
		} else {
			for name := range commonRequired {
				if !branchRequired[name] {
					delete(commonRequired, name)
				}
			}
		}
		if branch["additionalProperties"] != false {
			allAdditionalPropertiesFalse = false
		}
	}
	required := requiredSet(schema)
	for name := range commonRequired {
		required[name] = true
	}
	setRequired(schema, required)
	if allAdditionalPropertiesFalse {
		if _, exists := schema["additionalProperties"]; !exists {
			schema["additionalProperties"] = false
			changed = true
		}
	}
	return changed
}

func mergeObjectProperties(target, source map[string]any, conflictKeyword string) bool {
	sourceProperties, ok := source["properties"].(map[string]any)
	if !ok {
		return false
	}
	targetProperties, _ := target["properties"].(map[string]any)
	if targetProperties == nil {
		targetProperties = make(map[string]any)
		target["properties"] = targetProperties
	}
	changed := false
	for name, incoming := range sourceProperties {
		existing, exists := targetProperties[name]
		if !exists {
			targetProperties[name] = cloneSchemaValue(incoming)
			changed = true
			continue
		}
		if reflect.DeepEqual(existing, incoming) {
			continue
		}
		targetProperties[name] = mergePropertyAlternatives(existing, incoming, conflictKeyword)
		changed = true
	}
	return changed
}

func mergePropertyAlternatives(existing, incoming any, keyword string) map[string]any {
	branches := []any{cloneSchemaValue(existing)}
	if object, ok := existing.(map[string]any); ok {
		if values, ok := object[keyword].([]any); ok && len(object) == 1 {
			branches = append([]any(nil), values...)
		}
	}
	for _, branch := range branches {
		if reflect.DeepEqual(branch, incoming) {
			return map[string]any{keyword: branches}
		}
	}
	branches = append(branches, cloneSchemaValue(incoming))
	return map[string]any{keyword: branches}
}

func requiredSet(schema map[string]any) map[string]bool {
	set := make(map[string]bool)
	if required, ok := schema["required"].([]any); ok {
		for _, rawName := range required {
			if name, ok := rawName.(string); ok {
				set[name] = true
			}
		}
	}
	return set
}

func setRequired(schema map[string]any, required map[string]bool) {
	names := make([]string, 0, len(required))
	for name := range required {
		names = append(names, name)
	}
	sort.Strings(names)
	values := make([]any, len(names))
	for index, name := range names {
		values[index] = name
	}
	schema["required"] = values
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
		var schema map[string]any
		var format string
		if value, ok := tool["parameters"].(map[string]any); ok {
			schema, format = value, "parameters"
		} else if value, ok := tool["input_schema"].(map[string]any); ok {
			schema, format = value, "input_schema"
		} else if function, ok := tool["function"].(map[string]any); ok {
			if value, ok := function["parameters"].(map[string]any); ok {
				schema, format = value, "function.parameters"
			}
		}
		if schema == nil {
			continue
		}
		summaries = append(summaries, map[string]any{
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
	if schemaType, exists := schema["type"]; exists {
		switch schemaType := schemaType.(type) {
		case string:
			out["type"] = schemaType
		case []any:
			out["type_count"] = len(schemaType)
		}
	}
	if _, exists := schema["$ref"]; exists {
		out["has_ref"] = true
	}
	if required, ok := schema["required"].([]any); ok {
		out["required_count"] = len(required)
	}
	if additional, ok := schema["additionalProperties"].(bool); ok {
		out["additionalProperties"] = additional
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
		out["property_count"] = len(properties)
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
