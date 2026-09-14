package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var fallbackExcludedHeaders = map[string]bool{
	"host":                     true,
	"content-length":           true,
	"connection":               true,
	"accept-encoding":          true,
	"transfer-encoding":        true,
	"upgrade":                  true,
	"keep-alive":               true,
	"proxy-connection":         true,
	"te":                       true,
	"trailer":                  true,
	"authorization":            true,
	"x-api-key":                true,
	"x-userid":                 true,
	"x-xai-token-auth":         true,
	"x-grok-client-version":    true,
	"x-grok-client-identifier": true,
	"x-grok-client-mode":       true,
}

func fallbackTarget(baseURL, path string) string {
	baseURL = strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(baseURL, "/v1") && strings.HasPrefix(path, "/v1/") {
		baseURL = strings.TrimSuffix(baseURL, "/v1")
	}
	return baseURL + path
}

func replaceFallbackModel(body []byte, model string) []byte {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil || model == "" {
		return body
	}
	payload["model"] = model
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return encoded
}

func (g *Gateway) CheckFallback(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Provider string `json:"provider"`
	}
	if json.NewDecoder(r.Body).Decode(&input) != nil || (input.Provider != "openai" && input.Provider != "anthropic") {
		g.writeError(w, http.StatusBadRequest, "provider 必须是 openai 或 anthropic")
		return
	}
	config, err := g.store.GetFallbackConfig(r.Context())
	if err != nil {
		g.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	baseURL, key, model, path := config.OpenAIBaseURL, config.OpenAIKey, config.OpenAIModel, "/v1/chat/completions"
	payload := map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": "Reply OK."}}, "max_tokens": 1, "stream": false}
	if input.Provider == "anthropic" {
		baseURL, key, model, path = config.AnthropicBaseURL, config.AnthropicKey, config.AnthropicModel, "/v1/messages"
		payload = map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": "Reply OK."}}, "max_tokens": 1, "stream": false}
	}
	if baseURL == "" || key == "" || model == "" {
		g.writeError(w, http.StatusBadRequest, "该 fallback 配置不完整")
		return
	}
	body, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fallbackTarget(baseURL, path), bytes.NewReader(body))
	if err != nil {
		g.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if input.Provider == "anthropic" {
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := g.http.Do(req)
	if err != nil {
		g.writeError(w, http.StatusBadGateway, "检查请求失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		g.writeError(w, http.StatusBadGateway, "fallback 上游返回 HTTP "+strconv.Itoa(resp.StatusCode))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "provider": input.Provider, "model": model})
}

func (g *Gateway) proxyFallback(w http.ResponseWriter, r *http.Request, provider string, body []byte, start time.Time, requestMetrics responseMetrics) bool {
	if g.store == nil {
		return false
	}
	config, err := g.store.GetFallbackConfig(r.Context())
	if err != nil {
		return false
	}
	baseURL, key, model := config.OpenAIBaseURL, config.OpenAIKey, config.OpenAIModel
	if provider == "anthropic" {
		baseURL, key, model = config.AnthropicBaseURL, config.AnthropicKey, config.AnthropicModel
	}
	if baseURL == "" || key == "" || model == "" {
		return false
	}
	path := r.URL.Path
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	fallbackBody := replaceFallbackModel(body, model)
	if provider == "openai" && r.URL.Path == "/v1/chat/completions" {
		fallbackBody, _ = ensureChatStreamUsage(fallbackBody)
	}
	request, err := http.NewRequestWithContext(r.Context(), r.Method, fallbackTarget(baseURL, path), bytes.NewReader(fallbackBody))
	if err != nil {
		return false
	}
	copyHeaders(request.Header, r.Header, fallbackExcludedHeaders)
	if provider == "anthropic" {
		request.Header.Set("x-api-key", key)
		if request.Header.Get("anthropic-version") == "" {
			request.Header.Set("anthropic-version", "2023-06-01")
		}
	} else {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	response, err := g.http.Do(request)
	if err != nil {
		g.writeError(w, http.StatusBadGateway, "fallback 上游请求失败: "+err.Error())
		requestMetrics.ErrorReason = "fallback 上游请求失败"
		g.logFallback(r, model, path, http.StatusBadGateway, start, requestMetrics)
		return true
	}
	defer response.Body.Close()
	copyHeaders(w.Header(), response.Header, excludeRespHeaders)
	w.WriteHeader(response.StatusCode)
	metrics := streamCopyRawWithMetrics(w, response.Body, response.Header.Get("Content-Type"), start)
	metrics.Stream = requestMetrics.Stream
	if response.StatusCode >= 400 {
		metrics.ErrorReason = "fallback 上游错误"
	}
	g.logFallback(r, model, path, response.StatusCode, start, metrics)
	return true
}
