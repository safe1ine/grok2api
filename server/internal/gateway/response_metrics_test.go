package gateway

import (
	"strings"
	"testing"
	"time"

	"net/http/httptest"
)

func TestParseStreamRequest(t *testing.T) {
	t.Parallel()
	if !parseStreamRequest([]byte(`{"stream":true}`)) {
		t.Fatal("stream:true was not detected")
	}
	for _, body := range [][]byte{[]byte(`{"stream":false}`), []byte(`{}`), []byte(`invalid`)} {
		if parseStreamRequest(body) {
			t.Fatalf("unexpected stream detection for %s", body)
		}
	}
}

func TestObserveOpenAIUsageFormats(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		body   string
		input  int
		cached int
		output int
	}{
		{
			name:   "responses",
			body:   `{"usage":{"input_tokens":120,"output_tokens":30,"input_tokens_details":{"cached_tokens":80}}}`,
			input:  120,
			cached: 80,
			output: 30,
		},
		{
			name:   "chat completions",
			body:   `{"usage":{"prompt_tokens":50,"completion_tokens":12,"prompt_tokens_details":{"cached_tokens":20}}}`,
			input:  50,
			cached: 20,
			output: 12,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metrics := responseMetrics{}
			observeResponsePayload([]byte(tt.body), &metrics, time.Now())
			if metrics.InputTokens != tt.input || metrics.CachedInputTokens != tt.cached || metrics.OutputTokens != tt.output {
				t.Fatalf("metrics = %+v", metrics)
			}
		})
	}
}

func TestStreamCopyCollectsAnthropicUsageAndTTFT(t *testing.T) {
	t.Parallel()

	input := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":70,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":25}}` + "\n\n"
	recorder := httptest.NewRecorder()
	start := time.Now().Add(-50 * time.Millisecond)
	metrics := streamCopyWithNamespaceMappings(recorder, strings.NewReader(input), "text/event-stream", nil, start)
	if metrics.InputTokens != 100 || metrics.CachedInputTokens != 70 || metrics.OutputTokens != 25 {
		t.Fatalf("metrics = %+v", metrics)
	}
	if metrics.TTFTMs < 40 {
		t.Fatalf("TTFTMs = %d, want at least 40", metrics.TTFTMs)
	}
	if !strings.Contains(recorder.Body.String(), "hello") {
		t.Fatal("stream output was not forwarded")
	}
}

func TestStreamCopyCollectsResponsesUsage(t *testing.T) {
	t.Parallel()

	input := `data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":200,"output_tokens":40,"input_tokens_details":{"cached_tokens":150}}}}` + "\n\n"
	recorder := httptest.NewRecorder()
	metrics := streamCopyWithNamespaceMappings(recorder, strings.NewReader(input), "text/event-stream", nil, time.Now().Add(-20*time.Millisecond))
	if metrics.InputTokens != 200 || metrics.CachedInputTokens != 150 || metrics.OutputTokens != 40 || metrics.TTFTMs == 0 {
		t.Fatalf("metrics = %+v", metrics)
	}
}

func TestNonStreamCopyCollectsUsageAndFirstBodyLatency(t *testing.T) {
	t.Parallel()

	body := `{"usage":{"input_tokens":10,"output_tokens":5,"input_tokens_details":{"cached_tokens":4}},"output_text":"done"}`
	recorder := httptest.NewRecorder()
	metrics := streamCopyWithNamespaceMappings(recorder, strings.NewReader(body), "application/json", nil, time.Now().Add(-10*time.Millisecond))
	if metrics.InputTokens != 10 || metrics.CachedInputTokens != 4 || metrics.OutputTokens != 5 {
		t.Fatalf("metrics = %+v", metrics)
	}
	if metrics.TTFTMs == 0 {
		t.Fatal("non-stream TTFT was not captured")
	}
	if recorder.Body.String() != body {
		t.Fatalf("body changed: %s", recorder.Body.String())
	}
}
