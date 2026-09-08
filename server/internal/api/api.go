package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"grok2api/server/internal/auth"
	"grok2api/server/internal/billing"
	"grok2api/server/internal/config"
	"grok2api/server/internal/oauth"
	"grok2api/server/internal/pool"
	"grok2api/server/internal/store"
)

const maxSchedulingWeight = 1000

func validSchedulingWeight(weight int) bool {
	return weight >= 1 && weight <= maxSchedulingWeight
}

type Handler struct {
	cfg   *config.Config
	store *store.Store
	pool  *pool.Pool
	oauth *oauth.Client
	keys  *auth.KeyCache
}

func New(cfg *config.Config, s *store.Store, p *pool.Pool, oc *oauth.Client, kc *auth.KeyCache) *Handler {
	return &Handler{cfg: cfg, store: s, pool: p, oauth: oc, keys: kc}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func randomState() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------- 管理台登录 ----------

func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体错误")
		return
	}
	userOK := subtle.ConstantTimeCompare([]byte(in.Username), []byte(h.cfg.AdminUsername)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(in.Password), []byte(h.cfg.AdminPassword)) == 1
	if !userOK || !passOK {
		writeErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	token, err := auth.IssueAdminToken(h.cfg, in.Username, 24*time.Hour)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

// ---------- OAuth ----------

// OAuthAuthorizeURL 生成授权登录地址。
func (h *Handler) OAuthAuthorizeURL(w http.ResponseWriter, r *http.Request) {
	state := randomState()
	verifier, err := oauth.NewVerifier()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := h.store.CreateOAuthState(r.Context(), state, verifier, 10*time.Minute); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	authorizeURL := h.oauth.AuthorizeURL(state, oauth.Challenge(verifier))
	writeJSON(w, http.StatusOK, map[string]string{
		"authorize_url": authorizeURL,
		"state":         state,
		"redirect_uri":  h.cfg.XAIRedirectURI,
	})
}

// completeOAuth 完成授权码兑换（code + state → 账号落库）。
func (h *Handler) completeOAuth(w http.ResponseWriter, r *http.Request, code, state string) (map[string]any, int, string) {
	if code == "" || state == "" {
		return nil, http.StatusBadRequest, "缺少 code 或 state"
	}
	verifier, err := h.store.ConsumeOAuthState(r.Context(), state)
	if err != nil {
		return nil, http.StatusBadRequest, "state 无效或已过期"
	}
	tok, err := h.oauth.ExchangeCode(r.Context(), code, verifier)
	if err != nil {
		return nil, http.StatusBadGateway, "兑换 token 失败: " + err.Error()
	}
	email := oauth.EmailFromIDToken(tok.IDToken)
	if email == "" {
		// id_token 理论上必带 sub；这里只是纯兑底，避免 email 唯一索引冲突
		email = "unknown-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	subject := tok.UserID
	if subject == "" {
		subject = oauth.SubjectFromIDToken(tok.IDToken)
	}
	if subject == "" {
		subject = oauth.SubjectFromIDToken(tok.AccessToken)
	}
	id, weight, err := h.store.CreateAccount(r.Context(), email, subject, tok.RefreshToken)
	if err != nil {
		return nil, http.StatusInternalServerError, "保存账号失败: " + err.Error()
	}
	h.pool.AddAccountWithWeight(id, email, subject, tok.RefreshToken, weight)
	return map[string]any{"id": id, "email": email}, http.StatusOK, ""
}

// OAuthCallback 浏览器回调（本机能访问 localhost 时自动完成）。
func (h *Handler) OAuthCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	acc, status, errMsg := h.completeOAuth(w, r, code, state)
	if status != http.StatusOK {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`<html><body style="font-family:sans-serif;padding:40px"><h2 style="color:#c00">登录失败</h2><p>` + errMsg + `</p></body></html>`))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<html><body style="font-family:sans-serif;padding:40px"><h2 style="color:#0a0">登录成功</h2><p>账号已添加：` + acc["email"].(string) + `</p><p>可以关闭此页面回到管理台。</p></body></html>`))
}

// OAuthComplete 手动粘贴回调 URL 完成登录。
func (h *Handler) OAuthComplete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CallbackURL string `json:"callback_url"`
		Code        string `json:"code"`
		State       string `json:"state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体错误")
		return
	}

	code, state := in.Code, in.State
	if in.CallbackURL != "" {
		u, err := url.Parse(in.CallbackURL)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "callback_url 不是合法 URL")
			return
		}
		code = u.Query().Get("code")
		state = u.Query().Get("state")
	}

	acc, status, errMsg := h.completeOAuth(w, r, code, state)
	if status != http.StatusOK {
		writeErr(w, status, errMsg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": acc})
}

// ---------- 设备码登录（推荐，无需注册 OAuth 应用） ----------

type deviceFlow struct {
	mu sync.Mutex

	DeviceCode              string
	UserCode                string
	VerificationURIComplete string
	ExpiresAt               time.Time
	Interval                int
	State                   string // pending / complete / failed
	Error                   string
	AccountID               int64
	AccountEmail            string
}

var deviceFlows sync.Map // deviceCode -> *deviceFlow

// DeviceStart 发起设备码登录。
func (h *Handler) DeviceStart(w http.ResponseWriter, r *http.Request) {
	dc, err := h.oauth.RequestDeviceCode(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	interval := dc.Interval
	if interval <= 0 {
		interval = 5
	}
	expiresIn := dc.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 1800
	}
	flow := &deviceFlow{
		DeviceCode:              dc.DeviceCode,
		UserCode:                dc.UserCode,
		VerificationURIComplete: dc.VerificationURIComplete,
		ExpiresAt:               time.Now().Add(time.Duration(expiresIn) * time.Second),
		Interval:                interval,
		State:                   "pending",
	}
	deviceFlows.Store(dc.DeviceCode, flow)
	go h.pollDevice(flow)

	writeJSON(w, http.StatusOK, map[string]any{
		"device_code":               dc.DeviceCode,
		"user_code":                 dc.UserCode,
		"verification_uri":          dc.VerificationURI,
		"verification_uri_complete": dc.VerificationURIComplete,
		"expires_in":                expiresIn,
	})
}

// DeviceStatus 查询设备码登录进度。
func (h *Handler) DeviceStatus(w http.ResponseWriter, r *http.Request) {
	deviceCode := r.URL.Query().Get("device_code")
	v, ok := deviceFlows.Load(deviceCode)
	if !ok {
		writeErr(w, http.StatusNotFound, "未找到该 device_code")
		return
	}
	flow := v.(*deviceFlow)
	flow.mu.Lock()
	defer flow.mu.Unlock()

	out := map[string]any{"state": flow.State}
	if flow.Error != "" {
		out["error"] = flow.Error
	}
	if flow.State == "complete" {
		out["account"] = map[string]any{"id": flow.AccountID, "email": flow.AccountEmail}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) pollDevice(flow *deviceFlow) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	interval := flow.Interval
	for {
		if time.Now().After(flow.ExpiresAt) {
			flow.mu.Lock()
			flow.State = "failed"
			flow.Error = "授权码已过期"
			flow.mu.Unlock()
			return
		}
		time.Sleep(time.Duration(interval) * time.Second)

		tok, err := h.oauth.PollDeviceToken(ctx, flow.DeviceCode)
		if err == nil {
			email := oauth.EmailFromIDToken(tok.IDToken)
			if email == "" {
				email = "unknown-" + strconv.FormatInt(time.Now().UnixNano(), 10)
			}
			subject := tok.UserID
			if subject == "" {
				subject = oauth.SubjectFromIDToken(tok.IDToken)
			}
			if subject == "" {
				subject = oauth.SubjectFromIDToken(tok.AccessToken)
			}
			id, weight, err := h.store.CreateAccount(ctx, email, subject, tok.RefreshToken)
			if err != nil {
				flow.mu.Lock()
				flow.State = "failed"
				flow.Error = "保存账号失败: " + err.Error()
				flow.mu.Unlock()
				return
			}
			h.pool.AddAccountWithWeight(id, email, subject, tok.RefreshToken, weight)
			flow.mu.Lock()
			flow.State = "complete"
			flow.AccountID = id
			flow.AccountEmail = email
			flow.mu.Unlock()
			return
		}
		if errors.Is(err, oauth.ErrAuthorizationPending) {
			continue
		}
		if errors.Is(err, oauth.ErrSlowDown) {
			interval += 5
			continue
		}
		flow.mu.Lock()
		flow.State = "failed"
		flow.Error = err.Error()
		flow.mu.Unlock()
		return
	}
}

type accountView struct {
	store.AccountRecord
	SubscriptionTier      string     `json:"subscription_tier"`
	WeeklyUsedPercent     *float64   `json:"weekly_used_percent"`
	WeeklyResetAt         *time.Time `json:"weekly_reset_at"`
	ResetCreditsKnown     bool       `json:"reset_credits_known"`
	ResetCreditsAvailable int        `json:"reset_credits_available"`
	ResetCreditExpiresAt  *time.Time `json:"reset_credit_expires_at"`
}

func applyAccountState(v *accountView, state pool.AccountState) {
	v.Status = state.Status
	v.CooldownUntil = state.CooldownUntil
}

func applyAccountUsage(v *accountView, usage billing.Usage) {
	v.SubscriptionTier = usage.SubscriptionTier
	v.WeeklyUsedPercent = &usage.WeeklyUsedPercent
	if !usage.WeeklyResetAt.IsZero() {
		v.WeeklyResetAt = &usage.WeeklyResetAt
	}
	v.ResetCreditsKnown = !usage.ResetCreditsUpdatedAt.IsZero()
	if !v.ResetCreditsKnown {
		return
	}
	available := usage.AvailableResetCredits(time.Now())
	v.ResetCreditsAvailable = len(available)
	if len(available) > 0 && !available[0].ExpiresAt.IsZero() {
		v.ResetCreditExpiresAt = &available[0].ExpiresAt
	}
}

func (h *Handler) ListAccounts(w http.ResponseWriter, r *http.Request) {
	accs, err := h.store.ListAccounts(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]accountView, 0, len(accs))
	for _, a := range accs {
		v := accountView{AccountRecord: a}
		if a.SchedulingDisabled {
			v.Status = pool.StatusDisabled
			v.CooldownUntil = nil
		} else if state, ok := h.pool.AccountState(a.ID); ok {
			applyAccountState(&v, state)
		}
		if usage, ok := h.pool.BillingUsage(a.ID); ok {
			applyAccountUsage(&v, usage)
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func accountGroupName(value string) (string, bool) {
	name := strings.TrimSpace(value)
	return name, name != "" && len([]rune(name)) <= 50
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (h *Handler) ListAccountGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := h.store.ListAccountGroups(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, groups)
}

func (h *Handler) CreateAccountGroup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	name, ok := accountGroupName(in.Name)
	if !ok {
		writeErr(w, http.StatusBadRequest, "分组名称必须是 1 到 50 个字符")
		return
	}
	group, err := h.store.CreateAccountGroup(r.Context(), name)
	if err != nil {
		if isUniqueViolation(err) {
			writeErr(w, http.StatusConflict, "分组名称已存在")
		} else {
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusCreated, group)
}

func (h *Handler) RenameAccountGroup(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "无效的分组 id")
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	name, ok := accountGroupName(in.Name)
	if !ok {
		writeErr(w, http.StatusBadRequest, "分组名称必须是 1 到 50 个字符")
		return
	}
	found, err := h.store.RenameAccountGroup(r.Context(), id, name)
	if err != nil {
		if isUniqueViolation(err) {
			writeErr(w, http.StatusConflict, "分组名称已存在")
		} else {
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "分组不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) DeleteAccountGroup(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "无效的分组 id")
		return
	}
	found, err := h.store.DeleteAccountGroup(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrDefaultAccountGroup) {
			writeErr(w, http.StatusConflict, err.Error())
		} else {
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "分组不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) UpdateAccountGroup(w http.ResponseWriter, r *http.Request) {
	accountID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "无效的账号 id")
		return
	}
	var in struct {
		GroupID int64 `json:"group_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.GroupID <= 0 {
		writeErr(w, http.StatusBadRequest, "无效的分组 id")
		return
	}
	found, err := h.store.SetAccountGroup(r.Context(), accountID, in.GroupID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "账号或分组不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) RedeemAccountReset(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "无效的账号 id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	usage, err := h.pool.RedeemReset(ctx, id)
	if err != nil {
		switch {
		case errors.Is(err, pool.ErrAccountNotFound):
			writeErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, billing.ErrNoResetCredit):
			writeErr(w, http.StatusConflict, err.Error())
		default:
			writeErr(w, http.StatusBadGateway, "重置周限失败："+err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                  true,
		"weekly_used_percent": usage.WeeklyUsedPercent,
	})
}

func (h *Handler) DisableAccount(w http.ResponseWriter, r *http.Request) {
	h.setAccountSchedulingDisabled(w, r, true)
}

func (h *Handler) EnableAccount(w http.ResponseWriter, r *http.Request) {
	h.setAccountSchedulingDisabled(w, r, false)
}

func (h *Handler) setAccountSchedulingDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "无效的账号 id")
		return
	}
	found, err := h.store.SetAccountSchedulingDisabled(r.Context(), id, disabled)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	h.pool.SetSchedulingDisabled(id, disabled)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "scheduling_disabled": disabled})
}

func (h *Handler) UpdateAccountWeight(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "无效的账号 id")
		return
	}
	var in struct {
		Weight int `json:"scheduling_weight"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	if !validSchedulingWeight(in.Weight) {
		writeErr(w, http.StatusBadRequest, "账号权重必须是 1 到 1000 之间的整数")
		return
	}
	found, err := h.store.SetAccountSchedulingWeight(r.Context(), id, in.Weight)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	h.pool.SetWeight(id, in.Weight)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "scheduling_weight": in.Weight})
}

func (h *Handler) DeleteAccount(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "无效的账号 id")
		return
	}
	if err := h.store.DeleteAccount(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.pool.Remove(id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------- API Key ----------

func (h *Handler) ListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := h.store.ListKeys(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, keys)
}

func newAPIKey() (plain, hash, prefix string, err error) {
	raw := make([]byte, 24)
	if _, err = rand.Read(raw); err != nil {
		return "", "", "", err
	}
	plain = "sk-grok2api-" + hex.EncodeToString(raw)
	return plain, auth.HashKey(plain), plain[:16], nil
}

func (h *Handler) CreateKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.Name == "" {
		in.Name = "default"
	}

	plain, hash, prefix, err := newAPIKey()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成密钥失败")
		return
	}
	id, err := h.store.CreateKey(r.Context(), in.Name, plain, hash, prefix)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = h.keys.Reload(r.Context())
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": in.Name, "key": plain})
}

func (h *Handler) RevealKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "无效的 key id")
		return
	}
	plain, found, err := h.store.RevealKey(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrKeySecretUnavailable) {
			writeErr(w, http.StatusConflict, err.Error())
		} else {
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "Key 不存在")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"key": plain})
}

func (h *Handler) RegenerateKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "无效的 key id")
		return
	}
	plain, hash, prefix, err := newAPIKey()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成密钥失败")
		return
	}
	found, err := h.store.RegenerateKey(r.Context(), id, plain, hash, prefix)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "Key 不存在")
		return
	}
	if err := h.keys.Reload(r.Context()); err != nil {
		writeErr(w, http.StatusInternalServerError, "刷新密钥缓存失败")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"key": plain})
}

func (h *Handler) DeleteKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "无效的 key id")
		return
	}
	if err := h.store.DeleteKey(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = h.keys.Reload(r.Context())
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------- 调用记录 ----------

type logListResponse struct {
	Items      []store.CallLog `json:"items"`
	Limit      int             `json:"limit"`
	HasMore    bool            `json:"has_more"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

type logCursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        int64     `json:"id"`
}

func encodeLogCursor(log store.CallLog) string {
	data, _ := json.Marshal(logCursor{CreatedAt: log.CreatedAt, ID: log.ID})
	return base64.RawURLEncoding.EncodeToString(data)
}

func parseLogPagination(r *http.Request) (int, *store.CallLogCursor, error) {
	limit := 50
	if value := r.URL.Query().Get("limit"); value != "" {
		if n, err := strconv.Atoi(value); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	value := r.URL.Query().Get("cursor")
	if value == "" {
		return limit, nil, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return 0, nil, errors.New("无效的分页游标")
	}
	var cursor logCursor
	if err := json.Unmarshal(data, &cursor); err != nil || cursor.ID <= 0 || cursor.CreatedAt.IsZero() {
		return 0, nil, errors.New("无效的分页游标")
	}
	return limit, &store.CallLogCursor{CreatedAt: cursor.CreatedAt, ID: cursor.ID}, nil
}

func (h *Handler) ListLogs(w http.ResponseWriter, r *http.Request) {
	limit, cursor, err := parseLogPagination(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	logs, err := h.store.ListCallLogs(r.Context(), limit+1, cursor)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	hasMore := len(logs) > limit
	if hasMore {
		logs = logs[:limit]
	}
	response := logListResponse{Items: logs, Limit: limit, HasMore: hasMore}
	if hasMore && len(logs) > 0 {
		response.NextCursor = encodeLogCursor(logs[len(logs)-1])
	}
	writeJSON(w, http.StatusOK, response)
}
