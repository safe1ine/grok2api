package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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

func parseRetryAfter(resp *http.Response) time.Duration {
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
		if t, err := http.ParseTime(s); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
		}
	}
	return 60 * time.Second
}

// ---------- 通用透传 ----------

func (g *Gateway) Proxy(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body, _ := io.ReadAll(r.Body)
	model := parseModel(body)
	upstreamPath := r.URL.Path
	if r.URL.RawQuery != "" {
		upstreamPath += "?" + r.URL.RawQuery
	}

	var acct *pool.Account
	finalStatus := 0
	saw401 := map[int64]int{}

	for attempt := 0; attempt < 6; attempt++ {
		a, err := g.pool.Acquire()
		if err != nil {
			if errors.Is(err, pool.ErrAllCoolingDown) {
				select {
				case <-time.After(500 * time.Millisecond):
					continue
				case <-r.Context().Done():
					return
				}
			}
			break
		}
		acct = a

		token, err := g.pool.Token(r.Context(), a)
		if err != nil {
			if errors.Is(err, oauth.ErrInvalidGrant) {
				g.pool.MarkNeedRelogin(r.Context(), a)
			} else {
				// 临时错误（网络等）：短冷却后重试，不误杀账号
				g.pool.Release(a, time.Now().Add(30*time.Second))
			}
			continue
		}

		resp, err := g.doRequest(r, token, body, upstreamPath)
		if err != nil {
			g.pool.Release(a, time.Now())
			break
		}
		g.captureRateLimit(a, resp)

		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			a.Invalidate()
			saw401[a.ID]++
			if saw401[a.ID] >= 2 {
				// 刷新后仍 401：账号 API 权限可能已失效
				g.pool.MarkNeedRelogin(r.Context(), a)
			} else {
				g.pool.Release(a, time.Now())
			}
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			g.pool.Release(a, time.Now().Add(parseRetryAfter(resp)))
			continue
		}

		copyHeaders(w.Header(), resp.Header, excludeRespHeaders)
		w.WriteHeader(resp.StatusCode)
		finalStatus = resp.StatusCode
		g.streamCopy(w, resp.Body)
		resp.Body.Close()
		g.pool.Release(a, time.Now())
		break
	}

	if finalStatus == 0 {
		finalStatus = http.StatusServiceUnavailable
		g.writeError(w, finalStatus, "所有账号暂不可用或均在冷却中")
	}
	g.log(r, acct, model, upstreamPath, finalStatus, start)
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
	flusher, ok := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if ok {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
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
		io.Copy(w, resp.Body)
		g.log(r, acct, in.Model, "/v1/tts", resp.StatusCode, start)
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
	g.log(r, acct, in.Model, "/v1/tts", http.StatusOK, start)
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
	g.streamCopy(w, resp.Body)
	g.log(r, acct, "stt", "/v1/stt", resp.StatusCode, start)
}

// simpleUpstream 单次上游请求（不重试）。成功时由调用方 Release。
func (g *Gateway) simpleUpstream(r *http.Request, method, path string, body []byte) (*http.Response, *pool.Account, error) {
	a, err := g.pool.Acquire()
	if err != nil {
		return nil, nil, err
	}
	token, err := g.pool.Token(r.Context(), a)
	if err != nil {
		if errors.Is(err, oauth.ErrInvalidGrant) {
			g.pool.MarkNeedRelogin(r.Context(), a)
		} else {
			g.pool.Release(a, time.Now().Add(30*time.Second))
		}
		return nil, nil, fmt.Errorf("账号 token 刷新失败")
	}
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
		return nil, nil, err
	}
	g.captureRateLimit(a, resp)
	return resp, a, nil
}

// ---------- 限流头捕获 ----------

func atoiHeader(h http.Header, key string) int {
	v := strings.TrimSpace(h.Get(key))
	if v == "" {
		return 0
	}
	n, _ := strconv.Atoi(v)
	return n
}

func (g *Gateway) captureRateLimit(acct *pool.Account, resp *http.Response) {
	if acct == nil || resp == nil {
		return
	}
	limit := atoiHeader(resp.Header, "X-Ratelimit-Limit-Requests")
	remaining := atoiHeader(resp.Header, "X-Ratelimit-Remaining-Requests")
	tokenLimit := atoiHeader(resp.Header, "X-Ratelimit-Limit-Tokens")
	tokenRemaining := atoiHeader(resp.Header, "X-Ratelimit-Remaining-Tokens")
	if limit == 0 && remaining == 0 && tokenLimit == 0 && tokenRemaining == 0 {
		return
	}
	g.pool.UpdateRateLimit(acct.ID, limit, remaining, tokenLimit, tokenRemaining)
}

// ---------- 工具 ----------

func (g *Gateway) writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (g *Gateway) log(r *http.Request, acct *pool.Account, model, endpoint string, status int, start time.Time) {
	if status == 0 {
		return
	}
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
	l := store.CallLog{
		KeyID:     keyID,
		AccountID: accountID,
		Model:     model,
		Endpoint:  endpoint,
		Status:    status,
		LatencyMs: int(time.Since(start).Milliseconds()),
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
