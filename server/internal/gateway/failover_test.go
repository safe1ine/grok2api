package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"grok2api/server/internal/config"
	"grok2api/server/internal/pool"
)

func TestProxyNormalizesCustomToolsDescriptionsAndKeywordNamedProperties(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		tools := payload["tools"].([]any)
		if len(tools) != 2 {
			t.Fatalf("tools = %#v", tools)
		}
		for index, rawTool := range tools {
			tool := rawTool.(map[string]any)
			if tool["type"] != "function" || tool["description"] == "" {
				t.Fatalf("tool %d was not normalized: %#v", index, tool)
			}
		}
		properties := tools[1].(map[string]any)["parameters"].(map[string]any)["properties"].(map[string]any)
		if _, ok := properties["required"].(map[string]any); !ok {
			t.Fatalf("property named required was corrupted: %#v", properties["required"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":[{"type":"function_call","name":"patch","call_id":"call_1","arguments":"{\"input\":\"raw patch\"}"}],"usage":{"input_tokens":10,"output_tokens":2}}`)
	}))
	defer upstream.Close()

	p := pool.New(nil, nil)
	p.AddAccount(1, "a@x.com", "refresh-1")
	account, err := p.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	account.AccessToken = "access-a"
	account.ExpiresAt = time.Now().Add(time.Hour)
	p.Release(account, time.Now())

	g := New(&config.Config{XAIAPIBase: upstream.URL}, p, nil)
	requestBody := []byte(`{"model":"grok-4.6","tools":[
		{"type":"custom","name":"patch"},
		{"type":"function","name":"keywords","parameters":{"type":"object","properties":{"properties":{"type":"string"},"required":null}}}
	]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(requestBody))
	recorder := httptest.NewRecorder()
	g.Proxy(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	call := response["output"].([]any)[0].(map[string]any)
	if call["type"] != "custom_tool_call" || call["input"] != "raw patch" {
		t.Fatalf("custom response was not restored: %#v", call)
	}
}

func TestProxyRetriesOnceWithRelaxedRootSchemaAfterExplicitUpstreamError(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		schema := payload["tools"].([]any)[0].(map[string]any)["parameters"].(map[string]any)
		if _, exists := schema["anyOf"]; exists {
			t.Fatal("root anyOf reached upstream")
		}
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"code":"invalid-argument","error":"Failed to start sampling: [invalid_client_tool_schema] tool parameter root must be an object type"}`)
			return
		}
		if schema["additionalProperties"] != true || len(schema["required"].([]any)) != 0 {
			t.Fatalf("second-attempt schema was not relaxed: %#v", schema)
		}
		_, _ = io.WriteString(w, `{"usage":{"input_tokens":10,"output_tokens":2}}`)
	}))
	defer upstream.Close()

	p := pool.New(nil, nil)
	p.AddAccount(1, "a@x.com", "refresh-1")
	account, err := p.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	account.AccessToken = "access-a"
	account.ExpiresAt = time.Now().Add(time.Hour)
	p.Release(account, time.Now())

	g := New(&config.Config{XAIAPIBase: upstream.URL}, p, nil)
	requestBody := []byte(`{"model":"grok-4.6","tools":[{"type":"function","name":"Update","parameters":{"anyOf":[
		{"type":"object","properties":{"left":{"type":"string"}},"required":["left"]},
		{"type":"object","properties":{"right":{"type":"number"}},"required":["right"]},
		{"type":"null"}
	]}}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(requestBody))
	recorder := httptest.NewRecorder()
	g.Proxy(recorder, req)
	if recorder.Code != http.StatusOK || calls != 2 {
		t.Fatalf("status=%d calls=%d body=%s", recorder.Code, calls, recorder.Body.String())
	}
}

func TestProxyFinishesUpstreamAfterClientCancellation(t *testing.T) {
	upstreamStarted := make(chan struct{})
	releaseUpstream := make(chan struct{})
	upstreamCancelled := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(upstreamStarted)
		select {
		case <-releaseUpstream:
		case <-r.Context().Done():
			upstreamCancelled <- struct{}{}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":2}}}`+"\n\n")
	}))
	defer upstream.Close()

	p := pool.New(nil, nil)
	p.AddAccount(1, "a@x.com", "refresh-1")
	account, err := p.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	account.AccessToken = "access-a"
	account.ExpiresAt = time.Now().Add(time.Hour)
	p.Release(account, time.Now())

	g := New(&config.Config{XAIAPIBase: upstream.URL}, p, nil)
	clientContext, cancelClient := context.WithCancel(context.Background())
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		bytes.NewReader([]byte(`{"model":"grok-4.6","input":"hello","stream":true}`)),
	).WithContext(clientContext)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		g.Proxy(recorder, req)
		close(done)
	}()

	<-upstreamStarted
	cancelClient()
	select {
	case <-upstreamCancelled:
		t.Fatal("client cancellation reached the upstream request")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseUpstream)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("proxy did not finish after upstream completed")
	}
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"input_tokens":10`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestProxyRetriesDifferentAccountAfterSpendingLimitResponse(t *testing.T) {
	var mu sync.Mutex
	var authorizations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		attempt := len(authorizations)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if attempt == 1 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"personal-team-blocked:spending-limit","error":"You have run out of credits"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"response_1","output":[],"usage":{"input_tokens":10,"output_tokens":2}}`))
	}))
	defer upstream.Close()

	p := pool.New(nil, nil)
	p.AddAccount(1, "a@x.com", "refresh-1")
	p.AddAccount(2, "b@x.com", "refresh-2")
	for range 2 {
		a, err := p.Acquire()
		if err != nil {
			t.Fatal(err)
		}
		a.AccessToken = "access-" + a.Email
		a.ExpiresAt = time.Now().Add(time.Hour)
		p.Release(a, time.Now())
	}

	g := New(&config.Config{XAIAPIBase: upstream.URL}, p, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(`{"model":"grok-4.6","input":"hello"}`)))
	recorder := httptest.NewRecorder()
	g.Proxy(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	mu.Lock()
	got := append([]string(nil), authorizations...)
	mu.Unlock()
	if len(got) != 2 || got[0] == got[1] {
		t.Fatalf("authorizations = %v, want two different accounts", got)
	}
	var exhausted int
	for _, id := range []int64{1, 2} {
		state, _ := p.AccountState(id)
		if state.Status == pool.StatusExhausted {
			exhausted++
		}
	}
	if exhausted != 1 {
		t.Fatalf("exhausted accounts = %d, want 1", exhausted)
	}
}

func TestSimpleUpstreamRetriesDifferentAccountAfterSpendingLimit(t *testing.T) {
	var mu sync.Mutex
	var authorizations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		attempt := len(authorizations)
		mu.Unlock()
		if attempt == 1 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"personal-team-blocked:spending-limit","error":"You have run out of credits"}`))
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	p := pool.New(nil, nil)
	p.AddAccount(1, "a@x.com", "refresh-1")
	p.AddAccount(2, "b@x.com", "refresh-2")
	for range 2 {
		a, err := p.Acquire()
		if err != nil {
			t.Fatal(err)
		}
		a.AccessToken = "access-" + a.Email
		a.ExpiresAt = time.Now().Add(time.Hour)
		p.Release(a, time.Now())
	}

	g := New(&config.Config{XAIAPIBase: upstream.URL}, p, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/tts", bytes.NewReader(nil))
	resp, account, err := g.simpleUpstream(req, http.MethodPost, "/v1/tts", []byte(`{"text":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	defer p.Release(account, time.Now())
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("body = %q", body)
	}

	mu.Lock()
	got := append([]string(nil), authorizations...)
	mu.Unlock()
	if len(got) != 2 || got[0] == got[1] {
		t.Fatalf("authorizations = %v, want two different accounts", got)
	}

	var exhausted, active int
	for _, id := range []int64{1, 2} {
		state, ok := p.AccountState(id)
		if !ok {
			t.Fatalf("missing account %d", id)
		}
		switch state.Status {
		case pool.StatusExhausted:
			exhausted++
		case pool.StatusActive:
			active++
		}
	}
	if exhausted != 1 || active != 1 {
		t.Fatalf("exhausted=%d active=%d", exhausted, active)
	}
}

func TestSimpleUpstreamRetriesDifferentAccountAfter429(t *testing.T) {
	var mu sync.Mutex
	var authorizations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		attempt := len(authorizations)
		mu.Unlock()
		if attempt == 1 {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	p := pool.New(nil, nil)
	p.AddAccount(1, "a@x.com", "refresh-1")
	p.AddAccount(2, "b@x.com", "refresh-2")
	for range 2 {
		a, err := p.Acquire()
		if err != nil {
			t.Fatal(err)
		}
		a.AccessToken = "access-" + a.Email
		a.ExpiresAt = time.Now().Add(time.Hour)
		p.Release(a, time.Now())
	}

	g := New(&config.Config{XAIAPIBase: upstream.URL}, p, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/tts", bytes.NewReader(nil))
	resp, account, err := g.simpleUpstream(req, http.MethodPost, "/v1/tts", []byte(`{"text":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	defer p.Release(account, time.Now())
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("body = %q", body)
	}

	mu.Lock()
	got := append([]string(nil), authorizations...)
	mu.Unlock()
	if len(got) != 2 || got[0] == got[1] {
		t.Fatalf("authorizations = %v, want two different accounts", got)
	}

	var cooling, active int
	for _, id := range []int64{1, 2} {
		state, ok := p.AccountState(id)
		if !ok {
			t.Fatalf("missing account %d", id)
		}
		switch state.Status {
		case pool.StatusCooldown:
			cooling++
		case pool.StatusActive:
			active++
		}
	}
	if cooling != 1 || active != 1 {
		t.Fatalf("cooling=%d active=%d", cooling, active)
	}

	// Ensure the async billing hook is a no-op without a configured billing client.
	p.RefreshBilling(context.Background())
}
