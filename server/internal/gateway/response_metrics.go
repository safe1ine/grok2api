package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"
)

type responseMetrics struct {
	InputTokens            int
	CachedInputTokens      int
	OutputTokens           int
	TTFTMs                 int
	Stream                 bool
	ErrorReason            string
	UsageSeen              bool
	StreamCompleted        bool
	DownstreamDisconnected bool
	UpstreamReadError      bool

	firstBodyMs int
	ttftSet     bool
}

func parseStreamRequest(body []byte) bool {
	var request struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &request) == nil && request.Stream
}

func ensureChatStreamUsage(body []byte) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var request map[string]any
	if err := dec.Decode(&request); err != nil || request["stream"] != true {
		return body, false
	}
	options, _ := request["stream_options"].(map[string]any)
	if options == nil {
		options = make(map[string]any)
		request["stream_options"] = options
	}
	if options["include_usage"] == true {
		return body, false
	}
	options["include_usage"] = true
	updated, err := json.Marshal(request)
	if err != nil {
		return body, false
	}
	return updated, true
}

func (m *responseMetrics) markFirstBody(start time.Time) {
	if m.firstBodyMs == 0 {
		m.firstBodyMs = elapsedMilliseconds(start)
	}
}

func (m *responseMetrics) markGeneratedDelta(start time.Time) {
	if !m.ttftSet {
		m.TTFTMs = elapsedMilliseconds(start)
		m.ttftSet = true
	}
}

func (m *responseMetrics) finalizeTTFT() {
	if !m.ttftSet && m.firstBodyMs > 0 {
		m.TTFTMs = m.firstBodyMs
		m.ttftSet = true
	}
}

func elapsedMilliseconds(start time.Time) int {
	elapsed := int(time.Since(start).Milliseconds())
	if elapsed < 1 {
		return 1
	}
	return elapsed
}

func observeResponsePayload(data []byte, metrics *responseMetrics, start time.Time) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var payload any
	if err := dec.Decode(&payload); err != nil {
		if metrics.ErrorReason == "" {
			metrics.ErrorReason = classifySafeResponseErrorData(data)
		}
		return
	}
	observeUsage(payload, metrics)
	observeStreamCompletion(payload, metrics)
	if metrics.ErrorReason == "" {
		metrics.ErrorReason = classifySafeResponseError(payload)
	}
	if containsGeneratedDelta(payload) {
		metrics.markGeneratedDelta(start)
	}
}

func observeUsage(value any, metrics *responseMetrics) {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "usage" {
				observeUsageObject(child, metrics)
				continue
			}
			observeUsage(child, metrics)
		}
	case []any:
		for _, child := range value {
			observeUsage(child, metrics)
		}
	}
}

func observeUsageObject(value any, metrics *responseMetrics) {
	usage, ok := value.(map[string]any)
	if !ok {
		return
	}
	input, inputSeen := firstTokenCountPresent(usage, "input_tokens", "prompt_tokens")
	output, outputSeen := firstTokenCountPresent(usage, "output_tokens", "completion_tokens")
	cached, cachedSeen := firstTokenCountPresent(usage, "cache_read_input_tokens", "cached_tokens")
	for _, detailsKey := range []string{"input_tokens_details", "prompt_tokens_details"} {
		if details, ok := usage[detailsKey].(map[string]any); ok {
			if count, seen := firstTokenCountPresent(details, "cached_tokens"); seen {
				cached = maxInt(cached, count)
				cachedSeen = true
			}
		}
	}
	metrics.UsageSeen = metrics.UsageSeen || inputSeen || outputSeen || cachedSeen
	metrics.InputTokens = maxInt(metrics.InputTokens, input)
	metrics.CachedInputTokens = maxInt(metrics.CachedInputTokens, cached)
	metrics.OutputTokens = maxInt(metrics.OutputTokens, output)
}

func observeStreamCompletion(value any, metrics *responseMetrics) {
	root, ok := value.(map[string]any)
	if !ok {
		return
	}
	eventType, _ := root["type"].(string)
	if eventType == "response.completed" || eventType == "message_stop" {
		metrics.StreamCompleted = true
	}
}

func classifySafeResponseErrorData(data []byte) string {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var payload any
	if err := dec.Decode(&payload); err == nil {
		return classifySafeResponseError(payload)
	}
	if len(data) > 64<<10 {
		data = data[:64<<10]
	}
	return classifyKnownErrorText(string(data))
}

// classifySafeResponseError 只返回固定分类，不保存上游原始错误文本、Schema 路径或工具参数。
func classifySafeResponseError(payload any) string {
	if reason := classifyKnownErrorText(errorSignalText(payload)); reason != "" {
		return reason
	}
	if code := safeResponseErrorCode(payload); code != "" {
		return "上游错误：" + code
	}
	return ""
}

func classifyKnownErrorText(value string) string {
	text := strings.ToLower(value)
	switch {
	case strings.Contains(text, "personal-team-blocked:spending-limit") || strings.Contains(text, "run out of credits"):
		return "账号额度已耗尽"
	case strings.Contains(text, "tool_choice") && strings.Contains(text, "unknown variant"):
		return "工具选择格式不兼容"
	case strings.Contains(text, "tool_choice") && strings.Contains(text, "no tools"):
		return "工具选择与工具定义不匹配"
	case strings.Contains(text, "tool parameter root") || strings.Contains(text, "root schema is an anyof/oneof"):
		return "工具参数根 Schema 不是 object"
	case strings.Contains(text, "unknown variant `custom`") || strings.Contains(text, "unknown variant 'custom'"):
		return "自定义工具类型不受支持"
	case strings.Contains(text, "missing field `description`") || strings.Contains(text, "missing field 'description'"):
		return "工具描述缺失"
	case strings.Contains(text, "/properties/required") && strings.Contains(text, "standard_violation"):
		return "工具 Schema 属性定义无效"
	case strings.Contains(text, "unresolvable $ref") || strings.Contains(text, "key '$defs' not found"):
		return "工具 Schema 引用无法解析"
	case strings.Contains(text, "schema validation") || strings.Contains(text, "client_tool_schema"):
		return "工具 Schema 校验失败"
	case strings.Contains(text, "could not decrypt") || strings.Contains(text, "encrypted_content"):
		return "上游无法解密会话状态"
	case strings.Contains(text, "compaction blob"):
		return "上游无法解析压缩状态"
	case strings.Contains(text, "reasoning effort"):
		return "推理强度参数不受支持"
	case strings.Contains(text, "context length") || strings.Contains(text, "context_length") || strings.Contains(text, "too many tokens"):
		return "上下文长度超限"
	case strings.Contains(text, "rate limit") || strings.Contains(text, "rate_limit") || strings.Contains(text, "quota"):
		return "上游限流或额度不足"
	case strings.Contains(text, "model") && strings.Contains(text, "not found"):
		return "请求的模型不存在"
	case strings.Contains(text, "invalid request") || strings.Contains(text, "invalid-argument") || strings.Contains(text, "invalid argument"):
		return "请求参数不兼容"
	}
	return ""
}

func safeResponseErrorCode(payload any) string {
	root, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	candidates := []any{root["code"]}
	if nested, ok := root["error"].(map[string]any); ok {
		candidates = append(candidates, nested["code"], nested["type"])
	}
	for _, candidate := range candidates {
		code, ok := candidate.(string)
		if !ok || code == "" || len(code) > 80 {
			continue
		}
		valid := true
		for _, char := range code {
			if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
				(char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' {
				continue
			}
			valid = false
			break
		}
		if valid {
			return code
		}
	}
	return ""
}

func isToolRootSchemaErrorResponse(data []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var payload any
	text := ""
	if err := dec.Decode(&payload); err == nil {
		text = errorSignalText(payload)
	} else {
		if len(data) > 64<<10 {
			data = data[:64<<10]
		}
		text = string(data)
	}
	text = strings.ToLower(text)
	return strings.Contains(text, "invalid_client_tool_schema") &&
		(strings.Contains(text, "tool parameter root") || strings.Contains(text, "root schema"))
}

func isAccountQuotaExhaustedResponse(data []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var payload any
	if err := dec.Decode(&payload); err == nil {
		text := strings.ToLower(errorSignalText(payload))
		return strings.Contains(text, "personal-team-blocked:spending-limit") ||
			strings.Contains(text, "run out of credits")
	}
	if len(data) > 64<<10 {
		data = data[:64<<10]
	}
	text := strings.ToLower(string(data))
	return strings.Contains(text, "personal-team-blocked:spending-limit") ||
		strings.Contains(text, "run out of credits")
}

func errorSignalText(payload any) string {
	root, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, 4)
	for _, key := range []string{"code", "error", "message", "detail"} {
		if value, exists := root[key]; exists {
			collectErrorSignalStrings(value, &parts, 0)
		}
	}
	if root["type"] == "error" {
		collectErrorSignalStrings(root, &parts, 0)
	}
	return strings.Join(parts, " ")
}

func collectErrorSignalStrings(value any, parts *[]string, depth int) {
	if depth >= 6 || len(*parts) >= 32 {
		return
	}
	switch value := value.(type) {
	case string:
		if len(value) > 4096 {
			value = value[:4096]
		}
		*parts = append(*parts, value)
	case map[string]any:
		for _, key := range []string{"code", "type", "error", "message", "detail"} {
			if child, exists := value[key]; exists {
				collectErrorSignalStrings(child, parts, depth+1)
			}
		}
	case []any:
		for _, child := range value {
			collectErrorSignalStrings(child, parts, depth+1)
		}
	}
}

func firstTokenCount(values map[string]any, keys ...string) int {
	count, _ := firstTokenCountPresent(values, keys...)
	return count
}

func firstTokenCountPresent(values map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		value, exists := values[key]
		if !exists {
			continue
		}
		if count, ok := tokenCount(value); ok {
			return count, true
		}
	}
	return 0, false
}

func tokenCount(value any) (int, bool) {
	switch value := value.(type) {
	case json.Number:
		count, err := value.Int64()
		return maxInt(0, int(count)), err == nil
	case float64:
		return maxInt(0, int(value)), true
	case int:
		return maxInt(0, value), true
	case int64:
		return maxInt(0, int(value)), true
	default:
		return 0, false
	}
}

func containsGeneratedDelta(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		if delta, exists := value["delta"]; exists && containsOutputFragment(delta) {
			return true
		}
		for _, child := range value {
			if containsGeneratedDelta(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if containsGeneratedDelta(child) {
				return true
			}
		}
	}
	return false
}

func containsOutputFragment(value any) bool {
	switch value := value.(type) {
	case string:
		return value != ""
	case map[string]any:
		for _, key := range []string{"text", "content", "arguments", "partial_json", "refusal"} {
			if fragment, ok := value[key].(string); ok && fragment != "" {
				return true
			}
		}
		for _, key := range []string{"tool_calls", "function"} {
			if child, exists := value[key]; exists && containsOutputFragment(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if containsOutputFragment(child) {
				return true
			}
		}
	}
	return false
}

func readAllWithMetrics(body io.Reader, metrics *responseMetrics, start time.Time) []byte {
	var out bytes.Buffer
	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			metrics.markFirstBody(start)
			_, _ = out.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return out.Bytes()
}

func maxInt(left, right int) int {
	if right > left {
		return right
	}
	return left
}

func isEventStream(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}
