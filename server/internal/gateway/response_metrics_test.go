package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func TestEnsureChatStreamUsage(t *testing.T) {
	t.Parallel()

	body, changed := ensureChatStreamUsage([]byte(`{"model":"grok-4.6","stream":true,"seed":9007199254740993}`))
	if !changed {
		t.Fatal("streaming chat request was not changed")
	}
	var request struct {
		Seed          json.Number `json:"seed"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&request); err != nil {
		t.Fatal(err)
	}
	if !request.StreamOptions.IncludeUsage || request.Seed.String() != "9007199254740993" {
		t.Fatalf("request = %+v", request)
	}

	for _, input := range []string{
		`{"stream":false}`,
		`{"stream":true,"stream_options":{"include_usage":true}}`,
	} {
		if output, changed := ensureChatStreamUsage([]byte(input)); changed || string(output) != input {
			t.Fatalf("unexpected change: input=%s output=%s", input, output)
		}
	}
}

func TestUpstreamCompletionContextIgnoresClientCancellation(t *testing.T) {
	t.Parallel()

	clientContext, cancelClient := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(clientContext)
	request, cancelUpstream := withUpstreamCompletionContext(request)
	defer cancelUpstream()
	cancelClient()

	select {
	case <-request.Context().Done():
		t.Fatalf("upstream context was cancelled with client: %v", request.Context().Err())
	default:
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

func TestDetectToolRootSchemaErrorResponse(t *testing.T) {
	t.Parallel()

	matching := []byte(`{"code":"invalid-argument","error":"Failed to start sampling: [invalid_client_tool_schema] tool parameter root must be an object type"}`)
	if !isToolRootSchemaErrorResponse(matching) {
		t.Fatal("expected tool root schema error to match")
	}
	for _, body := range [][]byte{
		[]byte(`{"code":"invalid-argument","error":"ordinary invalid argument"}`),
		[]byte(`{"code":"invalid_client_tool_schema","error":"nested property failed"}`),
	} {
		if isToolRootSchemaErrorResponse(body) {
			t.Fatalf("unexpected match for %s", body)
		}
	}
}

func TestClassifySafeResponseErrorExtractsKnownReasons(t *testing.T) {
	t.Parallel()

	tests := []struct {
		body string
		want string
	}{
		{
			body: `{"code":"personal-team-blocked:spending-limit","error":"You have run out of credits or need a Grok subscription."}`,
			want: "账号额度已耗尽",
		},
		{
			body: `{"code":"invalid-argument","error":"Failed to deserialize: tool_choice.type: unknown variant none"}`,
			want: "工具选择格式不兼容",
		},
		{
			body: `{"code":"invalid-argument","error":"tool parameter root must be an object type (root schema is an anyOf/oneOf union with a non-object branch)"}`,
			want: "工具参数根 Schema 不是 object",
		},
		{
			body: `{"error":"tools[7].type: unknown variant ` + "`custom`" + `"}`,
			want: "自定义工具类型不受支持",
		},
		{
			body: `{"error":"tools[0]: missing field ` + "`description`" + `"}`,
			want: "工具描述缺失",
		},
		{
			body: `{"code":"invalid-argument","error":"Schema validation failed: [standard_violation] /properties/required: []"}`,
			want: "工具 Schema 属性定义无效",
		},
		{
			body: `{"code":"invalid-argument","error":"unresolvable $ref; key $defs not found"}`,
			want: "工具 Schema 引用无法解析",
		},
		{
			body: `{"error":{"code":"upstream_unavailable","message":"unclassified"}}`,
			want: "上游错误：upstream_unavailable",
		},
	}
	for _, test := range tests {
		if got := classifySafeResponseErrorData([]byte(test.body)); got != test.want {
			t.Errorf("body=%s got=%q want=%q", test.body, got, test.want)
		}
	}
}

func TestClassifySafeResponseErrorDoesNotRetainSensitiveDetails(t *testing.T) {
	t.Parallel()

	body := []byte(`{"code":"invalid-argument","error":"Schema validation failed at SECRET_PARAMETER with SECRET_PROMPT"}`)
	reason := classifySafeResponseErrorData(body)
	if reason != "工具 Schema 校验失败" {
		t.Fatalf("reason = %q", reason)
	}
	for _, secret := range []string{"SECRET_PARAMETER", "SECRET_PROMPT"} {
		if strings.Contains(reason, secret) {
			t.Fatalf("reason leaked %s: %s", secret, reason)
		}
	}
}

func TestObserveResponsePayloadCapturesErrorReason(t *testing.T) {
	t.Parallel()

	metrics := responseMetrics{}
	observeResponsePayload(
		[]byte(`{"code":"invalid-argument","error":"A tool_choice was set but no tools were specified"}`),
		&metrics,
		time.Now(),
	)
	if metrics.ErrorReason != "工具选择与工具定义不匹配" {
		t.Fatalf("error reason = %q", metrics.ErrorReason)
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

type failingResponseWriter struct {
	header http.Header
}

func (w *failingResponseWriter) Header() http.Header {
	return w.header
}

func (*failingResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("downstream disconnected")
}

func (*failingResponseWriter) WriteHeader(int) {}

func TestStreamCopyContinuesAfterDownstreamDisconnect(t *testing.T) {
	t.Parallel()

	input := `data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":200,"output_tokens":40,"input_tokens_details":{"cached_tokens":150}}}}` + "\n\n"
	writer := &failingResponseWriter{header: make(http.Header)}
	metrics := streamCopyWithNamespaceMappings(writer, strings.NewReader(input), "text/event-stream", nil, time.Now())
	if !metrics.DownstreamDisconnected || !metrics.StreamCompleted || !metrics.UsageSeen {
		t.Fatalf("metrics = %+v", metrics)
	}
	if metrics.InputTokens != 200 || metrics.CachedInputTokens != 150 || metrics.OutputTokens != 40 {
		t.Fatalf("metrics = %+v", metrics)
	}
}

func TestObserveUsageIgnoresFieldsOutsideUsageObject(t *testing.T) {
	t.Parallel()

	metrics := responseMetrics{}
	observeResponsePayload(
		[]byte(`{"output":{"input_tokens":999,"output_tokens":888},"usage":{"input_tokens":10,"output_tokens":5}}`),
		&metrics,
		time.Now(),
	)
	if metrics.InputTokens != 10 || metrics.OutputTokens != 5 {
		t.Fatalf("metrics = %+v", metrics)
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
