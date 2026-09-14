package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
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
	request, err := http.NewRequestWithContext(r.Context(), r.Method, fallbackTarget(baseURL, path), bytes.NewReader(replaceFallbackModel(body, model)))
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
		g.log(r, nil, model, path, http.StatusBadGateway, start, requestMetrics)
		return true
	}
	defer response.Body.Close()
	copyHeaders(w.Header(), response.Header, excludeRespHeaders)
	w.WriteHeader(response.StatusCode)
	metrics := streamCopyWithCompatibility(w, response.Body, response.Header.Get("Content-Type"), namespaceToolMappings{}, start, streamCompatibilityOptions{})
	metrics.Stream = requestMetrics.Stream
	if response.StatusCode >= 400 {
		metrics.ErrorReason = "fallback 上游错误"
	}
	g.log(r, nil, model, path, response.StatusCode, start, metrics)
	return true
}
