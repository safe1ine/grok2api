package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	clientVersion    = "1.0.5"
	clientIdentifier = "grok-shell"
	clientModeCLI    = "cli"
	grpcContentType  = "application/grpc-web+proto"
	maxResponseBody  = 1 << 20
)

var ErrNoResetCredit = errors.New("没有可用的周限重置机会")

type ResetCredit struct {
	TokenID   string
	ValidFrom time.Time
	ExpiresAt time.Time
}

func (c ResetCredit) AvailableAt(now time.Time) bool {
	return c.TokenID != "" && (c.ValidFrom.IsZero() || !now.Before(c.ValidFrom)) &&
		(c.ExpiresAt.IsZero() || now.Before(c.ExpiresAt))
}

type Usage struct {
	SubscriptionTier  string
	WeeklyUsedPercent float64
	WeeklyResetAt     time.Time
	UpdatedAt         time.Time

	// ResetCredits 只保存在服务内存中；TokenID 不会返回管理端。
	ResetCredits          []ResetCredit
	ResetCreditsUpdatedAt time.Time
	ResetCreditsError     string
}

func (u Usage) AvailableResetCredits(now time.Time) []ResetCredit {
	credits := make([]ResetCredit, 0, len(u.ResetCredits))
	for _, credit := range u.ResetCredits {
		if credit.AvailableAt(now) {
			credits = append(credits, credit)
		}
	}
	sort.SliceStable(credits, func(i, j int) bool {
		if credits[i].ExpiresAt.IsZero() {
			return false
		}
		if credits[j].ExpiresAt.IsZero() {
			return true
		}
		return credits[i].ExpiresAt.Before(credits[j].ExpiresAt)
	})
	return credits
}

type Client struct {
	baseURL    string
	webBaseURL string
	http       *http.Client
}

func New(baseURL, webBaseURL string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		webBaseURL: strings.TrimRight(webBaseURL, "/"),
		http:       &http.Client{Timeout: 20 * time.Second},
	}
}

func (c *Client) Fetch(ctx context.Context, accessToken string) (Usage, error) {
	return c.FetchForUser(ctx, accessToken, "")
}

func (c *Client) FetchForUser(ctx context.Context, accessToken, userID string) (Usage, error) {
	var payload struct {
		Config struct {
			CreditUsagePercent   *float64 `json:"creditUsagePercent"`
			IsUnifiedBillingUser *bool    `json:"isUnifiedBillingUser"`
			BillingPeriodEnd     string   `json:"billingPeriodEnd"`
			CurrentPeriod        struct {
				Type  string `json:"type"`
				Start string `json:"start"`
				End   string `json:"end"`
			} `json:"currentPeriod"`
		} `json:"config"`
	}
	if err := c.getJSON(ctx, "/billing?format=credits", accessToken, userID, "billing", &payload); err != nil {
		return Usage{}, err
	}
	if payload.Config.CurrentPeriod.Type != "USAGE_PERIOD_TYPE_WEEKLY" {
		return Usage{}, fmt.Errorf("billing 返回了非周周期 %q", payload.Config.CurrentPeriod.Type)
	}
	// xAI 在人工重置后、尚未产生新用量时可能省略或返回 null；官方网页将其按 0% 处理。
	// 明确处于非统一计费模式时不作这个推断，避免把不适用的配置误判为额度恢复。
	used := 0.0
	if payload.Config.CreditUsagePercent == nil {
		if payload.Config.IsUnifiedBillingUser != nil && !*payload.Config.IsUnifiedBillingUser {
			return Usage{}, errors.New("billing 响应缺少周用量百分比且账号未启用统一计费")
		}
	} else {
		if math.IsNaN(*payload.Config.CreditUsagePercent) || math.IsInf(*payload.Config.CreditUsagePercent, 0) {
			return Usage{}, errors.New("billing 响应包含无效的周用量百分比")
		}
		used = *payload.Config.CreditUsagePercent
	}
	resetValue := payload.Config.CurrentPeriod.End
	if resetValue == "" {
		resetValue = payload.Config.BillingPeriodEnd
	}
	resetAt, err := time.Parse(time.RFC3339Nano, resetValue)
	if err != nil {
		return Usage{}, fmt.Errorf("billing 响应包含无效重置时间: %w", err)
	}
	now := time.Now()
	used = min(100, max(0, used))
	usage := Usage{WeeklyUsedPercent: used, WeeklyResetAt: resetAt, UpdatedAt: now}

	var settings struct {
		SubscriptionTierDisplay string `json:"subscription_tier_display"`
	}
	if err := c.getJSON(ctx, "/settings", accessToken, userID, "", &settings); err == nil {
		usage.SubscriptionTier = settings.SubscriptionTierDisplay
	}

	// 重置券是附加能力：查询失败不能影响周用量，也不能把最后一次成功结果覆盖成 0。
	if credits, err := c.FetchResetCreditsForUser(ctx, accessToken, userID); err == nil {
		usage.ResetCredits = credits
		usage.ResetCreditsUpdatedAt = now
	} else {
		usage.ResetCreditsError = err.Error()
	}
	return usage, nil
}

func (c *Client) FetchResetCredits(ctx context.Context, accessToken string) ([]ResetCredit, error) {
	return c.FetchResetCreditsForUser(ctx, accessToken, "")
}

func (c *Client) FetchResetCreditsForUser(ctx context.Context, accessToken, userID string) ([]ResetCredit, error) {
	body, headers, err := c.postGRPC(ctx, "/prod_mc_billing.ConsumerUiSvc/GetRemainingResets", accessToken, userID, grpcFrame(nil))
	if err != nil {
		return nil, err
	}
	message, trailerStatus, sawData, err := decodeGRPCWeb(body)
	if err != nil {
		return nil, fmt.Errorf("解析 Grok 重置机会响应失败: %w", err)
	}
	status := trailerStatus
	if status == nil {
		status = grpcStatusHeader(headers)
	}
	if status != nil && *status != 0 {
		return nil, fmt.Errorf("Grok 重置机会 RPC 返回 grpc-status %d", *status)
	}
	if !sawData {
		return nil, errors.New("Grok 重置机会 RPC 未返回数据帧")
	}
	credits, err := decodeResetCredits(message)
	if err != nil {
		return nil, fmt.Errorf("解析 Grok 重置机会 protobuf 失败: %w", err)
	}
	return credits, nil
}

func (c *Client) RedeemReset(ctx context.Context, accessToken, tokenID string) error {
	return c.RedeemResetForUser(ctx, accessToken, "", tokenID)
}

func (c *Client) RedeemResetForUser(ctx context.Context, accessToken, userID, tokenID string) error {
	if tokenID == "" {
		return ErrNoResetCredit
	}
	request := appendProtoBytes(nil, 10, []byte(tokenID))
	body, headers, err := c.postGRPC(ctx, "/prod_mc_billing.ConsumerUiSvc/RedeemReset", accessToken, userID, grpcFrame(request))
	if err != nil {
		return err
	}
	_, trailerStatus, _, err := decodeGRPCWeb(body)
	if err != nil {
		return fmt.Errorf("解析 Grok 重置响应失败: %w", err)
	}
	status := trailerStatus
	if status == nil {
		status = grpcStatusHeader(headers)
	}
	// RedeemReset 可能返回 HTTP 200 + 空 body，成功与否只能由 grpc-status 确认。
	if status == nil {
		return errors.New("Grok 重置响应缺少 grpc-status")
	}
	if *status != 0 {
		return fmt.Errorf("Grok 重置失败（grpc-status %d）", *status)
	}
	return nil
}

func (c *Client) getJSON(ctx context.Context, path, accessToken, userID, mode string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	applyCLIIdentity(req, accessToken, userID, mode)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("请求 Grok %s 失败: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("Grok %s 返回 HTTP %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(out); err != nil {
		return fmt.Errorf("解析 Grok %s 响应失败: %w", path, err)
	}
	return nil
}

func (c *Client) postGRPC(ctx context.Context, path, accessToken, userID string, body []byte) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.webBaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	applyCLIIdentity(req, accessToken, userID, clientModeCLI)
	req.Header.Set("Content-Type", grpcContentType)
	req.Header.Set("Accept", grpcContentType+", application/json")
	req.Header.Set("X-Grpc-Web", "1")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Origin", c.webBaseURL)
	req.Header.Set("Referer", c.webBaseURL+"/")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("请求 Grok %s 失败: %w", path, err)
	}
	defer resp.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if readErr != nil {
		return nil, nil, fmt.Errorf("读取 Grok %s 响应失败: %w", path, readErr)
	}
	if len(responseBody) > maxResponseBody {
		return nil, nil, fmt.Errorf("Grok %s 响应过大", path)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("Grok %s 返回 HTTP %d", path, resp.StatusCode)
	}
	return responseBody, resp.Header.Clone(), nil
}

func applyCLIIdentity(req *http.Request, accessToken, userID, mode string) {
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	req.Header.Set("x-grok-client-version", clientVersion)
	req.Header.Set("x-grok-client-identifier", clientIdentifier)
	if userID != "" {
		req.Header.Set("x-userid", userID)
	}
	if mode != "" {
		req.Header.Set("x-grok-client-mode", mode)
	}
	req.Header.Set("User-Agent", "xai-grok-workspace/"+clientVersion)
}

func grpcFrame(message []byte) []byte {
	frame := make([]byte, 5, 5+len(message))
	length := len(message)
	frame[1] = byte(length >> 24)
	frame[2] = byte(length >> 16)
	frame[3] = byte(length >> 8)
	frame[4] = byte(length)
	return append(frame, message...)
}

func decodeGRPCWeb(data []byte) (message []byte, trailerStatus *int, sawData bool, err error) {
	for offset := 0; offset < len(data); {
		if len(data)-offset < 5 {
			return nil, nil, false, errors.New("grpc-web 帧头不完整")
		}
		flags := data[offset]
		length := int(data[offset+1])<<24 | int(data[offset+2])<<16 | int(data[offset+3])<<8 | int(data[offset+4])
		offset += 5
		if length < 0 || length > len(data)-offset {
			return nil, nil, false, errors.New("grpc-web 帧内容不完整")
		}
		payload := data[offset : offset+length]
		offset += length
		if flags&0x80 != 0 {
			if status := grpcStatusText(string(payload)); status != nil {
				trailerStatus = status
			}
			continue
		}
		sawData = true
		message = payload
	}
	return message, trailerStatus, sawData, nil
}

func grpcStatusHeader(headers http.Header) *int {
	return grpcStatusText(headers.Get("grpc-status"))
}

func grpcStatusText(value string) *int {
	for _, line := range strings.FieldsFunc(value, func(r rune) bool { return r == '\r' || r == '\n' }) {
		line = strings.TrimSpace(line)
		if before, after, ok := strings.Cut(line, ":"); ok {
			if !strings.EqualFold(strings.TrimSpace(before), "grpc-status") {
				continue
			}
			line = strings.TrimSpace(after)
		}
		if code, err := strconv.Atoi(line); err == nil {
			return &code
		}
	}
	return nil
}

type protoField struct {
	number int
	wire   int
	bytes  []byte
	value  uint64
}

func decodeProtoFields(data []byte) ([]protoField, error) {
	fields := make([]protoField, 0)
	for offset := 0; offset < len(data); {
		tag, next, ok := readVarint(data, offset)
		if !ok {
			return nil, errors.New("protobuf tag 无效")
		}
		offset = next
		field := protoField{number: int(tag >> 3), wire: int(tag & 7)}
		switch field.wire {
		case 0:
			value, end, ok := readVarint(data, offset)
			if !ok {
				return nil, errors.New("protobuf varint 无效")
			}
			field.value = value
			offset = end
		case 1:
			if len(data)-offset < 8 {
				return nil, errors.New("protobuf fixed64 不完整")
			}
			offset += 8
		case 2:
			length, end, ok := readVarint(data, offset)
			if !ok || length > uint64(len(data)-end) {
				return nil, errors.New("protobuf bytes 不完整")
			}
			offset = end
			field.bytes = data[offset : offset+int(length)]
			offset += int(length)
		case 5:
			if len(data)-offset < 4 {
				return nil, errors.New("protobuf fixed32 不完整")
			}
			offset += 4
		default:
			return nil, fmt.Errorf("不支持的 protobuf wire type %d", field.wire)
		}
		fields = append(fields, field)
	}
	return fields, nil
}

func decodeResetCredits(message []byte) ([]ResetCredit, error) {
	root, err := decodeProtoFields(message)
	if err != nil {
		return nil, err
	}
	credits := make([]ResetCredit, 0)
	for _, field := range root {
		if field.number != 10 || field.wire != 2 {
			continue
		}
		tokenFields, err := decodeProtoFields(field.bytes)
		if err != nil {
			return nil, err
		}
		var credit ResetCredit
		for _, tokenField := range tokenFields {
			switch {
			case tokenField.number == 10 && tokenField.wire == 2:
				credit.TokenID = string(tokenField.bytes)
			case tokenField.number == 20 && tokenField.wire == 2:
				credit.ValidFrom, err = decodeTimestamp(tokenField.bytes)
			case tokenField.number == 30 && tokenField.wire == 2:
				credit.ExpiresAt, err = decodeTimestamp(tokenField.bytes)
			}
			if err != nil {
				return nil, err
			}
		}
		if credit.TokenID != "" {
			credits = append(credits, credit)
		}
	}
	return credits, nil
}

func decodeTimestamp(message []byte) (time.Time, error) {
	fields, err := decodeProtoFields(message)
	if err != nil {
		return time.Time{}, err
	}
	var seconds int64
	var nanos int64
	for _, field := range fields {
		if field.wire != 0 {
			continue
		}
		switch field.number {
		case 1:
			seconds = int64(field.value)
		case 2:
			nanos = int64(field.value)
		}
	}
	return time.Unix(seconds, nanos), nil
}

func appendProtoBytes(dst []byte, fieldNumber int, value []byte) []byte {
	dst = appendVarint(dst, uint64(fieldNumber<<3|2))
	dst = appendVarint(dst, uint64(len(value)))
	return append(dst, value...)
}

func appendVarint(dst []byte, value uint64) []byte {
	for value >= 0x80 {
		dst = append(dst, byte(value&0x7f)|0x80)
		value >>= 7
	}
	return append(dst, byte(value))
}

func readVarint(data []byte, offset int) (uint64, int, bool) {
	var value uint64
	for shift := uint(0); shift < 64 && offset < len(data); shift += 7 {
		b := data[offset]
		offset++
		value |= uint64(b&0x7f) << shift
		if b < 0x80 {
			return value, offset, true
		}
	}
	return 0, offset, false
}
