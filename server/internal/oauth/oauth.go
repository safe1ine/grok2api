package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrInvalidGrant 表示 refresh_token 已永久失效（需重新登录），区别于临时网络错误。
var (
	ErrInvalidGrant         = errors.New("invalid_grant")
	ErrAuthorizationPending = errors.New("authorization_pending")
	ErrSlowDown             = errors.New("slow_down")
)

type Client struct {
	AuthBase     string
	ClientID     string
	ClientSecret string // 可选
	RedirectURI  string
	Scope        string
	HTTP         *http.Client
}

type Token struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	IDToken      string `json:"id_token"`
	UserID       string `json:"user_id"`
}

func New(authBase, clientID, clientSecret, redirectURI, scope string) *Client {
	return &Client{
		AuthBase:     strings.TrimRight(authBase, "/"),
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURI:  redirectURI,
		Scope:        scope,
		HTTP:         &http.Client{Timeout: 30 * time.Second},
	}
}

// AuthorizeURL 生成授权码登录地址。
func (c *Client) AuthorizeURL(state, codeChallenge string) string {
	q := url.Values{}
	q.Set("client_id", c.ClientID)
	q.Set("redirect_uri", c.RedirectURI)
	q.Set("response_type", "code")
	q.Set("scope", c.Scope)
	q.Set("state", state)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	return c.AuthBase + "/oauth2/authorize?" + q.Encode()
}

func (c *Client) postForm(ctx context.Context, path string, form url.Values) (*Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.AuthBase+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if c.ClientSecret != "" {
		req.SetBasicAuth(c.ClientID, c.ClientSecret)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &e)
		msg := e.Error
		if e.ErrorDescription != "" {
			msg = msg + ": " + e.ErrorDescription
		}
		if msg == "" {
			msg = "HTTP " + resp.Status
		}
		switch e.Error {
		case "invalid_grant", "invalid_token", "unauthorized_client":
			return nil, fmt.Errorf("%w: %s", ErrInvalidGrant, msg)
		}
		return nil, fmt.Errorf("oauth token 请求失败: %s", msg)
	}
	var tok Token
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("解析 token 响应失败: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, errors.New("token 响应缺少 access_token")
	}
	return &tok, nil
}

// ExchangeCode 用授权码换 token。
func (c *Client) ExchangeCode(ctx context.Context, code, verifier string) (*Token, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", c.ClientID)
	form.Set("redirect_uri", c.RedirectURI)
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	return c.postForm(ctx, "/oauth2/token", form)
}

// Refresh 用 refresh_token 换新 token。
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*Token, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", c.ClientID)
	form.Set("refresh_token", refreshToken)
	return c.postForm(ctx, "/oauth2/token", form)
}

type idTokenClaims struct {
	Email string `json:"email"`
	Sub   string `json:"sub"`
}

func claimsFromIDToken(idToken string) idTokenClaims {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return idTokenClaims{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return idTokenClaims{}
	}
	var claims idTokenClaims
	_ = json.Unmarshal(payload, &claims)
	return claims
}

// EmailFromIDToken 从 id_token 的 payload 里取 email（不验签，仅展示用）。
func EmailFromIDToken(idToken string) string {
	claims := claimsFromIDToken(idToken)
	if claims.Email != "" {
		return claims.Email
	}
	return claims.Sub
}

// SubjectFromIDToken 从 id_token 的 payload 里取 xAI 用户 ID。
func SubjectFromIDToken(idToken string) string {
	return claimsFromIDToken(idToken).Sub
}

// ---------- 设备码流程（RFC 8628） ----------

type DeviceCode struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// RequestDeviceCode 发起设备码授权。
func (c *Client) RequestDeviceCode(ctx context.Context) (*DeviceCode, error) {
	form := url.Values{}
	form.Set("client_id", c.ClientID)
	form.Set("scope", c.Scope)
	form.Set("referrer", "grok2api")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.AuthBase+"/oauth2/device/code", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("设备码请求失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}
	var dc DeviceCode
	if err := json.Unmarshal(body, &dc); err != nil {
		return nil, fmt.Errorf("解析设备码响应失败: %w", err)
	}
	return &dc, nil
}

// PollDeviceToken 轮询设备码授权结果。
// 返回 ErrAuthorizationPending（继续等）、ErrSlowDown（降低频率继续等）、其他错误（失败）。
func (c *Client) PollDeviceToken(ctx context.Context, deviceCode string) (*Token, error) {
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	form.Set("client_id", c.ClientID)
	form.Set("device_code", deviceCode)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.AuthBase+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusOK {
		var tok Token
		if err := json.Unmarshal(body, &tok); err != nil {
			return nil, fmt.Errorf("解析 token 响应失败: %w", err)
		}
		if tok.AccessToken == "" {
			return nil, errors.New("token 响应缺少 access_token")
		}
		return &tok, nil
	}

	var e struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &e)
	switch e.Error {
	case "authorization_pending":
		return nil, ErrAuthorizationPending
	case "slow_down":
		return nil, ErrSlowDown
	}
	msg := e.Error
	if e.ErrorDescription != "" {
		msg = msg + ": " + e.ErrorDescription
	}
	if msg == "" {
		msg = "HTTP " + resp.Status
	}
	return nil, fmt.Errorf("设备码轮询失败: %s", msg)
}
