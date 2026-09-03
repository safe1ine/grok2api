package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"grok2api/server/internal/auth"
	"grok2api/server/internal/config"
	"grok2api/server/internal/oauth"
	"grok2api/server/internal/pool"
	"grok2api/server/internal/store"
)

const upstreamCompletionTimeout = 30 * time.Minute

type Gateway struct {
	cfg   *config.Config
	pool  *pool.Pool
	store *store.Store
	http  *http.Client

	modelsMu     sync.Mutex
	modelsLoadMu sync.Mutex // 拉取 /models 的单飞锁，避免缓存失效时惊群
	models       []byte
	modelsExp    time.Time
}

func New(cfg *config.Config, p *pool.Pool, s *store.Store) *Gateway {
	return &Gateway{
		cfg:   cfg,
		pool:  p,
		store: s,
		http: &http.Client{
			Timeout: 0, // 流式长连接，不设总超时
			Transport: &http.Transport{
				MaxIdleConns:        200,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

var excludeReqHeaders = map[string]bool{
	"host": true, "content-length": true, "connection": true,
	"accept-encoding": true, "authorization": true, "x-api-key": true,
	"transfer-encoding": true, "upgrade": true, "keep-alive": true,
	"proxy-connection": true, "te": true, "trailer": true,
}

var excludeRespHeaders = map[string]bool{
	"content-length": true, "connection": true, "transfer-encoding": true,
	"keep-alive": true, "upgrade": true,
}

func copyHeaders(dst http.Header, src http.Header, exclude map[string]bool) {
	for k, vs := range src {
		if exclude[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func parseModel(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &m)
	return m.Model
}

func accountAttemptLimit(p *pool.Pool) int {
	count := p.Len()
	if count < 1 {
		return 1
	}
	return min(count, 6)
}

// ---------- 通用透传 ----------

func withUpstreamCompletionContext(r *http.Request) (*http.Request, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), upstreamCompletionTimeout)
	return r.WithContext(ctx), cancel
}

func (g *Gateway) Proxy(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body, _ := io.ReadAll(r.Body)
	if r.URL.Path == "/v1/chat/completions" {
		body, _ = ensureChatStreamUsage(body)
	}
	r, cancelUpstream := withUpstreamCompletionContext(r)
	defer cancelUpstream()
	roleChanged := false
	anthropicToolChoiceChanged := false
	if r.URL.Path == "/v1/messages" {
		body, roleChanged = normalizeAnthropicMessageRoles(body)
		body, anthropicToolChoiceChanged = normalizeAnthropicToolChoice(body)
	}
	body, namespaceMappings, namespaceChanged := flattenNamespaceTools(body)
	body, schemaChanged := normalizeToolSchemas(body)
	body, toolChoiceChanged, invalidToolChoice := normalizeToolChoiceWithoutTools(body)
	compatibilityChanged := roleChanged || anthropicToolChoiceChanged || namespaceChanged || schemaChanged || toolChoiceChanged
	model := parseModel(body)
	metrics := responseMetrics{Stream: parseStreamRequest(body)}
	upstreamPath := r.URL.Path
	if r.URL.RawQuery != "" {
		upstreamPath += "?" + r.URL.RawQuery
	}
	if invalidToolChoice {
		const message = "请求设置了 required 或指定函数的 tool_choice，但没有提供可用的 tools"
		g.writeError(w, http.StatusBadRequest, message)
		metrics.ErrorReason = "工具选择与工具定义不匹配"
		g.log(r, nil, model, upstreamPath, http.StatusBadRequest, start, metrics)
		return
	}

	var acct *pool.Account
	finalStatus := 0
	lastFailureReason := ""
	triedAccounts := map[int64]struct{}{}
	appliedStateFallbacks := map[stateCompatibilityIssue]bool{}
	appliedUnsupportedArguments := map[string]bool{}
	appliedRootSchemaFallback := false
	compatibilityRetries := 0
	maxAccountAttempts := accountAttemptLimit(g.pool)

accountsLoop:
	for accountAttempt := 0; accountAttempt < maxAccountAttempts; accountAttempt++ {
		a, err := g.pool.AcquireExcluding(triedAccounts)
		if err != nil {
			break
		}
		triedAccounts[a.ID] = struct{}{}
		acct = a

		token, err := g.pool.Token(r.Context(), a)
		if err != nil {
			if errors.Is(err, oauth.ErrInvalidGrant) {
				lastFailureReason = "账号需要重新授权"
				g.pool.MarkNeedRelogin(r.Context(), a)
			} else {
				lastFailureReason = "账号 Token 刷新失败"
				g.pool.Release(a, time.Now().Add(30*time.Second))
			}
			continue
		}

		refreshedAfter401 := false
		for {
			resp, err := g.doRequest(r, token, body, upstreamPath)
			if err != nil {
				lastFailureReason = "连接上游失败"
				g.pool.Release(a, time.Now())
				if r.Context().Err() != nil {
					break accountsLoop
				}
				continue accountsLoop
			}
			var errorBody []byte
			var errorBodyReadErr error
			if resp.StatusCode >= 400 {
				errorBody, errorBodyReadErr = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				resp.Body = io.NopCloser(bytes.NewReader(errorBody))
				if errorBodyReadErr == nil && isAccountQuotaExhaustedResponse(errorBody) {
					lastFailureReason = "账号额度已耗尽"
					resp.Body.Close()
					g.pool.ReleaseQuotaExhausted(a)
					g.pool.RefreshBillingAsync(a.ID)
					continue accountsLoop
				}
			}
			if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity {
				if errorBodyReadErr == nil {
					if compatibilityRetries < 4 && !appliedRootSchemaFallback && isToolRootSchemaErrorResponse(errorBody) {
						if fallbackBody, changed := relaxToolParameterRoots(body); changed {
							log.Printf("上游工具根 Schema 降级: path=%s account=%d", upstreamPath, a.ID)
							body = fallbackBody
							compatibilityChanged = true
							appliedRootSchemaFallback = true
							compatibilityRetries++
							continue
						}
					}
					issue := classifyStateCompatibilityError(errorBody)
					if compatibilityRetries < 4 && issue != "" && !appliedStateFallbacks[issue] {
						before := stateCompatibilitySummary(body)
						if fallbackBody, changed := applyStateCompatibilityFallback(body, issue); changed {
							if encoded, err := json.Marshal(before); err == nil {
								log.Printf("上游状态兼容降级: path=%s issue=%s account=%d state=%s", upstreamPath, issue, a.ID, encoded)
							}
							body = fallbackBody
							compatibilityChanged = true
							appliedStateFallbacks[issue] = true
							compatibilityRetries++
							continue
						}
					}
					argument := unsupportedArgument(errorBody)
					if compatibilityRetries < 4 && argument != "" && !appliedUnsupportedArguments[argument] {
						if fallbackBody, changed := removeUnsupportedArgument(body, argument); changed {
							log.Printf("上游参数兼容降级: path=%s argument=%s account=%d", upstreamPath, argument, a.ID)
							body = fallbackBody
							compatibilityChanged = true
							appliedUnsupportedArguments[argument] = true
							compatibilityRetries++
							continue
						}
					}

					lowerError := strings.ToLower(string(errorBody))
					if strings.Contains(lowerError, "schema validation") {
						if summary := toolSchemaSummary(body); summary != "" {
							log.Printf("上游 Schema 失败: path=%s normalized=%t schema=%s", upstreamPath, compatibilityChanged, summary)
						}
					}
					if strings.Contains(lowerError, "invalid message role") {
						if summary := messageRoleSummary(body); summary != nil {
							if encoded, err := json.Marshal(summary); err == nil {
								log.Printf("上游 Message Roles 失败: path=%s normalized=%t roles=%s", upstreamPath, compatibilityChanged, encoded)
							}
						}
					}
				}
			}

			if resp.StatusCode == http.StatusUnauthorized {
				if reason := classifySafeResponseErrorData(errorBody); reason != "" {
					lastFailureReason = reason
				}
				resp.Body.Close()
				if refreshedAfter401 {
					if lastFailureReason == "" {
						lastFailureReason = "上游账号认证失败"
					}
					g.pool.MarkNeedRelogin(r.Context(), a)
					continue accountsLoop
				}
				a.Invalidate()
				token, err = g.pool.Token(r.Context(), a)
				if err != nil {
					if errors.Is(err, oauth.ErrInvalidGrant) {
						lastFailureReason = "账号需要重新授权"
						g.pool.MarkNeedRelogin(r.Context(), a)
					} else {
						lastFailureReason = "账号 Token 刷新失败"
						g.pool.Release(a, time.Now().Add(30*time.Second))
					}
					continue accountsLoop
				}
				refreshedAfter401 = true
				continue
			}
			if resp.StatusCode == http.StatusTooManyRequests {
				if reason := classifySafeResponseErrorData(errorBody); reason != "" {
					lastFailureReason = reason
				}
				resp.Body.Close()
				g.pool.Release(a, time.Now())
				g.pool.RefreshBillingAsync(a.ID)
				continue accountsLoop
			}

			copyHeaders(w.Header(), resp.Header, excludeRespHeaders)
			w.WriteHeader(resp.StatusCode)
			finalStatus = resp.StatusCode
			responseStats := streamCopyWithCompatibility(
				w, resp.Body, resp.Header.Get("Content-Type"), namespaceMappings, start,
				streamCompatibilityOptions{
					fillAnthropicIndexes:    r.URL.Path == "/v1/messages",
					normalizeAnthropicUsage: r.URL.Path == "/v1/messages",
				},
			)
			responseStats.Stream = metrics.Stream
			metrics = responseStats
			resp.Body.Close()
			g.pool.Release(a, time.Now())
			break accountsLoop
		}
	}

	if finalStatus == 0 {
		finalStatus = http.StatusServiceUnavailable
		if lastFailureReason == "" {
			lastFailureReason = "所有账号暂不可用或均在冷却、额度耗尽状态"
		}
		metrics.ErrorReason = lastFailureReason
		g.writeError(w, finalStatus, "所有账号暂不可用或均在冷却、额度耗尽状态")
	}
	g.log(r, acct, model, upstreamPath, finalStatus, start, metrics)
}

func (g *Gateway) doRequest(r *http.Request, token string, body []byte, upstreamPath string) (*http.Response, error) {
	url := g.cfg.XAIAPIBase + upstreamPath
	req, err := http.NewRequestWithContext(r.Context(), r.Method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyHeaders(req.Header, r.Header, excludeReqHeaders)
	req.Header.Set("Authorization", "Bearer "+token)
	return g.http.Do(req)
}

func (g *Gateway) streamCopy(w http.ResponseWriter, body io.Reader) {
	streamCopyRaw(w, body)
}

// ---------- /models 聚合 ----------

func (g *Gateway) HandleModels(w http.ResponseWriter, r *http.Request) {
	// 快速路径：命中缓存直接返回
	g.modelsMu.Lock()
	if time.Now().Before(g.modelsExp) && g.models != nil {
		data := g.models
		g.modelsMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
		return
	}
	g.modelsMu.Unlock()

	// 单飞：同一时刻只有一个请求去上游拉取
	g.modelsLoadMu.Lock()
	defer g.modelsLoadMu.Unlock()

	// 拿到锁后再次检查缓存
	g.modelsMu.Lock()
	if time.Now().Before(g.modelsExp) && g.models != nil {
		data := g.models
		g.modelsMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
		return
	}
	g.modelsMu.Unlock()

	merged := map[string]any{}
	const maxProbe = 10
	for i := 0; i < maxProbe; i++ {
		a, err := g.pool.Acquire()
		if err != nil {
			break
		}
		token, err := g.pool.Token(r.Context(), a)
		if err != nil {
			if errors.Is(err, oauth.ErrInvalidGrant) {
				g.pool.MarkNeedRelogin(r.Context(), a)
			} else {
				g.pool.Release(a, time.Now().Add(30*time.Second))
			}
			continue
		}
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, g.cfg.XAIAPIBase+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := g.http.Do(req)
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				var parsed struct {
					Data []map[string]any `json:"data"`
				}
				if json.NewDecoder(resp.Body).Decode(&parsed) == nil {
					for _, m := range parsed.Data {
						if id, ok := m["id"].(string); ok {
							merged[id] = m
						}
					}
				}
			}
			resp.Body.Close()
		}
		g.pool.Release(a, time.Now())
	}

	if len(merged) == 0 {
		g.writeError(w, http.StatusServiceUnavailable, "没有可用账号或上游 /models 无结果")
		return
	}
	data := make([]any, 0, len(merged))
	for _, m := range merged {
		data = append(data, m)
	}
	out, _ := json.Marshal(map[string]any{"object": "list", "data": data})

	g.modelsMu.Lock()
	g.models = out
	g.modelsExp = time.Now().Add(60 * time.Second)
	g.modelsMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

// ---------- TTS：OpenAI /audio/speech → xAI /tts ----------

type ttsReq struct {
	Model          string  `json:"model"`
	Input          string  `json:"input"`
	Voice          string  `json:"voice"`
	ResponseFormat string  `json:"response_format"`
	Speed          float64 `json:"speed"`
}

func (g *Gateway) HandleTTS(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body, _ := io.ReadAll(r.Body)
	var in ttsReq
	if err := json.Unmarshal(body, &in); err != nil {
		g.writeError(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}

	codec := in.ResponseFormat
	if codec == "" {
		codec = "mp3"
	}
	payload := map[string]any{
		"text":     in.Input,
		"voice_id": orDefault(in.Voice, "eve"),
		"language": "auto",
		"output_format": map[string]any{
			"codec":       codec,
			"sample_rate": 24000,
		},
	}
	if in.Speed != 0 {
		payload["speed"] = in.Speed
	}
	payloadBytes, _ := json.Marshal(payload)

	resp, acct, err := g.simpleUpstream(r, http.MethodPost, "/v1/tts", payloadBytes)
	if err != nil {
		g.writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer g.pool.Release(acct, time.Now())
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		copyHeaders(w.Header(), resp.Header, excludeRespHeaders)
		w.WriteHeader(resp.StatusCode)
		metrics := streamCopyWithCompatibility(
			w, resp.Body, resp.Header.Get("Content-Type"), nil, start, streamCompatibilityOptions{},
		)
		g.log(r, acct, in.Model, "/v1/tts", resp.StatusCode, start, metrics)
		return
	}

	var out struct {
		Audio       string `json:"audio"`
		ContentType string `json:"content_type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		g.writeError(w, http.StatusBadGateway, "解析 TTS 响应失败: "+err.Error())
		return
	}
	raw, err := base64.StdEncoding.DecodeString(out.Audio)
	if err != nil {
		g.writeError(w, http.StatusBadGateway, "解码音频失败: "+err.Error())
		return
	}
	ct := orDefault(out.ContentType, "audio/mpeg")
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.WriteHeader(http.StatusOK)
	w.Write(raw)
	g.log(r, acct, in.Model, "/v1/tts", http.StatusOK, start, responseMetrics{})
}

// ---------- STT：OpenAI /audio/transcriptions → xAI /stt（multipart 原样转发） ----------

func (g *Gateway) HandleSTT(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body, _ := io.ReadAll(r.Body)
	resp, acct, err := g.simpleUpstream(r, http.MethodPost, "/v1/stt", body)
	if err != nil {
		g.writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer g.pool.Release(acct, time.Now())
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header, excludeRespHeaders)
	w.WriteHeader(resp.StatusCode)
	metrics := streamCopyWithCompatibility(
		w, resp.Body, resp.Header.Get("Content-Type"), nil, start, streamCompatibilityOptions{},
	)
	g.log(r, acct, "stt", "/v1/stt", resp.StatusCode, start, metrics)
}

// simpleUpstream 用于 TTS/STT；401 刷新一次，429 或账号失效时切换到其他账号。
// 成功返回时由调用方 Release。
func (g *Gateway) simpleUpstream(r *http.Request, method, path string, body []byte) (*http.Response, *pool.Account, error) {
	triedAccounts := map[int64]struct{}{}
	maxAccountAttempts := accountAttemptLimit(g.pool)

accountsLoop:
	for accountAttempt := 0; accountAttempt < maxAccountAttempts; accountAttempt++ {
		a, err := g.pool.AcquireExcluding(triedAccounts)
		if err != nil {
			break
		}
		triedAccounts[a.ID] = struct{}{}
		token, err := g.pool.Token(r.Context(), a)
		if err != nil {
			if errors.Is(err, oauth.ErrInvalidGrant) {
				g.pool.MarkNeedRelogin(r.Context(), a)
			} else {
				g.pool.Release(a, time.Now().Add(30*time.Second))
			}
			continue
		}

		refreshedAfter401 := false
		for {
			req, err := http.NewRequestWithContext(r.Context(), method, g.cfg.XAIAPIBase+path, bytes.NewReader(body))
			if err != nil {
				g.pool.Release(a, time.Now())
				return nil, nil, err
			}
			copyHeaders(req.Header, r.Header, excludeReqHeaders)
			req.Header.Set("Authorization", "Bearer "+token)

			resp, err := g.http.Do(req)
			if err != nil {
				g.pool.Release(a, time.Now())
				if r.Context().Err() != nil {
					return nil, nil, err
				}
				continue accountsLoop
			}
			if resp.StatusCode >= 400 {
				errorBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				resp.Body = io.NopCloser(bytes.NewReader(errorBody))
				if readErr == nil && isAccountQuotaExhaustedResponse(errorBody) {
					resp.Body.Close()
					g.pool.ReleaseQuotaExhausted(a)
					g.pool.RefreshBillingAsync(a.ID)
					continue accountsLoop
				}
			}
			if resp.StatusCode == http.StatusUnauthorized {
				resp.Body.Close()
				if refreshedAfter401 {
					g.pool.MarkNeedRelogin(r.Context(), a)
					continue accountsLoop
				}
				a.Invalidate()
				token, err = g.pool.Token(r.Context(), a)
				if err != nil {
					if errors.Is(err, oauth.ErrInvalidGrant) {
						g.pool.MarkNeedRelogin(r.Context(), a)
					} else {
						g.pool.Release(a, time.Now().Add(30*time.Second))
					}
					continue accountsLoop
				}
				refreshedAfter401 = true
				continue
			}
			if resp.StatusCode == http.StatusTooManyRequests {
				resp.Body.Close()
				g.pool.Release(a, time.Now())
				g.pool.RefreshBillingAsync(a.ID)
				continue accountsLoop
			}
			return resp, a, nil
		}
	}
	return nil, nil, errors.New("所有账号暂不可用或均在冷却、额度耗尽状态")
}

// ---------- 工具 ----------

func (g *Gateway) writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (g *Gateway) log(
	r *http.Request,
	acct *pool.Account,
	model, endpoint string,
	status int,
	start time.Time,
	metrics responseMetrics,
) {
	if status == 0 || g.store == nil {
		return
	}
	if status >= 200 && status < 300 && metrics.Stream &&
		(metrics.DownstreamDisconnected || metrics.UpstreamReadError || !metrics.UsageSeen) {
		accountID := int64(0)
		if acct != nil {
			accountID = acct.ID
		}
		log.Printf(
			"流式调用终止诊断: endpoint=%s account=%d usage_seen=%t completed=%t downstream_disconnected=%t upstream_read_error=%t",
			endpoint, accountID, metrics.UsageSeen, metrics.StreamCompleted,
			metrics.DownstreamDisconnected, metrics.UpstreamReadError,
		)
	}
	totalLatencyMs := int(time.Since(start).Milliseconds())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var keyID, accountID *int64
	if id, ok := auth.KeyIDFromContext(r); ok {
		keyID = &id
	}
	if acct != nil {
		id := acct.ID
		accountID = &id
		g.store.TouchLastUsed(ctx, id)
	}
	errorReason := ""
	if status < 200 || status >= 300 {
		errorReason = metrics.ErrorReason
	}
	l := store.CallLog{
		KeyID:            keyID,
		AccountID:        accountID,
		Model:            model,
		Endpoint:         endpoint,
		Status:           status,
		ErrorReason:      errorReason,
		PromptTokens:     metrics.InputTokens,
		CachedTokens:     metrics.CachedInputTokens,
		CompletionTokens: metrics.OutputTokens,
		TTFTMs:           metrics.TTFTMs,
		LatencyMs:        totalLatencyMs,
		Stream:           metrics.Stream,
	}
	if err := g.store.InsertCallLog(ctx, l); err != nil {
		log.Printf("写入调用记录失败: %v", err)
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
