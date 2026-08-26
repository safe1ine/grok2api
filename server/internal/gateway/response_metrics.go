package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"
)

type responseMetrics struct {
	InputTokens       int
	CachedInputTokens int
	OutputTokens      int
	TTFTMs            int
	Stream            bool

	firstBodyMs int
	ttftSet     bool
}

func parseStreamRequest(body []byte) bool {
	var request struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &request) == nil && request.Stream
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
		return
	}
	observeUsage(payload, metrics)
	if containsGeneratedDelta(payload) {
		metrics.markGeneratedDelta(start)
	}
}

func observeUsage(value any, metrics *responseMetrics) {
	switch value := value.(type) {
	case map[string]any:
		input := firstTokenCount(value, "input_tokens", "prompt_tokens")
		output := firstTokenCount(value, "output_tokens", "completion_tokens")
		cached := firstTokenCount(value, "cache_read_input_tokens", "cached_tokens")
		for _, detailsKey := range []string{"input_tokens_details", "prompt_tokens_details"} {
			if details, ok := value[detailsKey].(map[string]any); ok {
				cached = maxInt(cached, firstTokenCount(details, "cached_tokens"))
			}
		}
		metrics.InputTokens = maxInt(metrics.InputTokens, input)
		metrics.CachedInputTokens = maxInt(metrics.CachedInputTokens, cached)
		metrics.OutputTokens = maxInt(metrics.OutputTokens, output)
		for _, child := range value {
			observeUsage(child, metrics)
		}
	case []any:
		for _, child := range value {
			observeUsage(child, metrics)
		}
	}
}

func firstTokenCount(values map[string]any, keys ...string) int {
	for _, key := range keys {
		if count := tokenCount(values[key]); count > 0 {
			return count
		}
	}
	return 0
}

func tokenCount(value any) int {
	switch value := value.(type) {
	case json.Number:
		count, _ := value.Int64()
		return int(count)
	case float64:
		return int(value)
	case int:
		return value
	case int64:
		return int(value)
	default:
		return 0
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
