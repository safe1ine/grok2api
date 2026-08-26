package pool

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"grok2api/server/internal/billing"
	"grok2api/server/internal/oauth"
	"grok2api/server/internal/store"
)

var (
	ErrNoAccount       = errors.New("没有可用账号")
	ErrAllCoolingDown  = errors.New("所有账号都在冷却中")
	ErrAccountNotFound = errors.New("账号不存在或未启用")
)

type Account struct {
	ID            int64
	Email         string
	Status        string
	RefreshToken  string // 解密后的明文
	AccessToken   string
	ExpiresAt     time.Time
	CooldownUntil time.Time

	// Grok 订阅周用量（内存缓存，由 billing endpoint 定期刷新）。
	BillingUsage billing.Usage

	mu      sync.Mutex
	resetMu sync.Mutex // 序列化同一账号的重置券兑换，避免重复消费。

	// inFlight 和 lastAssigned 仅由 Pool.mu 保护，用于并发最少优先的账号选择。
	inFlight     int
	lastAssigned uint64
}

type Pool struct {
	store   *store.Store
	oauth   *oauth.Client
	billing *billing.Client

	mu      sync.Mutex
	byID    map[int64]*Account
	pickSeq uint64
}

func New(s *store.Store, oc *oauth.Client) *Pool {
	return &Pool{store: s, oauth: oc, byID: map[int64]*Account{}}
}

func (p *Pool) SetBillingClient(client *billing.Client) {
	p.mu.Lock()
	p.billing = client
	p.mu.Unlock()
}

// Reload 从数据库全量载入账号（仅 active 状态）。
func (p *Pool) Reload(ctx context.Context) error {
	recs, err := p.store.ListPoolAccounts(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.byID = map[int64]*Account{}
	p.pickSeq = 0
	now := time.Now()
	for _, r := range recs {
		a := &Account{
			ID:            r.ID,
			Email:         r.Email,
			Status:        r.Status,
			RefreshToken:  r.RefreshToken,
			CooldownUntil: now,
		}
		p.byID[r.ID] = a
	}
	return nil
}

// AddAccount 新增账号；若 id 已存在（重新授权）则原地更新 refresh_token。
func (p *Pool) AddAccount(id int64, email, refreshToken string) {
	p.mu.Lock()
	if a, ok := p.byID[id]; ok {
		a.mu.Lock()
		a.RefreshToken = refreshToken
		a.Email = email
		a.Status = "active"
		a.AccessToken = "" // 旧 access_token 作废
		a.mu.Unlock()
	} else {
		p.byID[id] = &Account{
			ID:            id,
			Email:         email,
			Status:        "active",
			RefreshToken:  refreshToken,
			CooldownUntil: time.Now(),
		}
	}
	p.mu.Unlock()
	p.refreshBillingAsync(id)
}

// Acquire 选择一个未冷却且当前并发最少的账号。
// 账号不会被独占移出池，同一账号可以同时服务多个请求。
func (p *Pool) Acquire() (*Account, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.byID) == 0 {
		return nil, ErrNoAccount
	}

	now := time.Now()
	var best *Account
	for _, a := range p.byID {
		if a.CooldownUntil.After(now) {
			continue
		}
		if best == nil || a.inFlight < best.inFlight ||
			(a.inFlight == best.inFlight && a.lastAssigned < best.lastAssigned) {
			best = a
		}
	}
	if best == nil {
		return nil, ErrAllCoolingDown
	}

	p.pickSeq++
	best.inFlight++
	best.lastAssigned = p.pickSeq
	return best, nil
}

// Release 结束一次账号租用；cooldownUntil 为下次可分配的最早时间。
// 并发场景下已有冷却只能延长，不能被其他成功请求的 Release 提前清除。
func (p *Pool) Release(a *Account, cooldownUntil time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	current, ok := p.byID[a.ID]
	if !ok || current != a {
		return // 已被删除或重新加载
	}
	if a.inFlight > 0 {
		a.inFlight--
	}
	if cooldownUntil.After(a.CooldownUntil) {
		a.CooldownUntil = cooldownUntil
	}
}

// Remove 从池中移除账号。
func (p *Pool) Remove(id int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.byID, id)
}

// Invalidate 让账号缓存的 access_token 失效，下次 Token() 会强制刷新。
func (a *Account) Invalidate() {
	a.mu.Lock()
	a.AccessToken = ""
	a.mu.Unlock()
}

// MarkNeedRelogin 标记需要重新授权并移除。
func (p *Pool) MarkNeedRelogin(ctx context.Context, a *Account) {
	_ = p.store.SetAccountStatus(ctx, a.ID, "need_relogin")
	p.Remove(a.ID)
}

// Token 返回账号可用 access_token，必要时刷新（账号级加锁）。
// 刷新失败会原样返回错误：调用方可用 errors.Is(err, oauth.ErrInvalidGrant) 区分永久失效与临时错误。
func (p *Pool) Token(ctx context.Context, a *Account) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.AccessToken != "" && time.Until(a.ExpiresAt) > 5*time.Minute {
		return a.AccessToken, nil
	}

	tok, err := p.oauth.Refresh(ctx, a.RefreshToken)
	if err != nil {
		return "", err
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	exp := tok.ExpiresIn
	if exp <= 0 {
		exp = 3600
	}
	a.ExpiresAt = time.Now().Add(time.Duration(exp) * time.Second)

	// 回写轮换后的 refresh_token（失败仅记日志，不影响本次请求）
	if err := p.store.UpdateRefreshToken(ctx, a.ID, a.RefreshToken); err != nil {
		log.Printf("回写账号 %d refresh_token 失败: %v", a.ID, err)
	}
	return a.AccessToken, nil
}

// Len 返回池中账号总数。
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.byID)
}

// RefreshBilling 刷新所有账号的订阅等级、周用量和重置时间。
func (p *Pool) RefreshBilling(ctx context.Context) {
	p.mu.Lock()
	ids := make([]int64, 0, len(p.byID))
	for id := range p.byID {
		ids = append(ids, id)
	}
	p.mu.Unlock()

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.refreshBillingAccount(ctx, id); err != nil {
				log.Printf("刷新账号 %d Grok 周用量失败: %v", id, err)
			}
		}()
	}
	wg.Wait()
}

// BillingUsage 返回账号最近一次成功获取的真实订阅周用量。
func (p *Pool) BillingUsage(id int64) (billing.Usage, bool) {
	p.mu.Lock()
	a, ok := p.byID[id]
	p.mu.Unlock()
	if !ok {
		return billing.Usage{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	usage := cloneBillingUsage(a.BillingUsage)
	return usage, !usage.UpdatedAt.IsZero()
}

// RedeemReset 重新查询账号的有效重置券，消费最早过期的一张，并刷新账号用量。
func (p *Pool) RedeemReset(ctx context.Context, id int64) (billing.Usage, error) {
	p.mu.Lock()
	a, ok := p.byID[id]
	client := p.billing
	p.mu.Unlock()
	if !ok || client == nil {
		return billing.Usage{}, ErrAccountNotFound
	}

	a.resetMu.Lock()
	defer a.resetMu.Unlock()

	accessToken, err := p.Token(ctx, a)
	if err != nil {
		return billing.Usage{}, err
	}
	credits, err := client.FetchResetCredits(ctx, accessToken)
	if err != nil {
		return billing.Usage{}, err
	}
	now := time.Now()
	freshResetUsage := billing.Usage{ResetCredits: credits, ResetCreditsUpdatedAt: now}
	available := freshResetUsage.AvailableResetCredits(now)
	if len(available) == 0 {
		a.mu.Lock()
		a.BillingUsage.ResetCredits = append([]billing.ResetCredit(nil), credits...)
		a.BillingUsage.ResetCreditsUpdatedAt = now
		usage := cloneBillingUsage(a.BillingUsage)
		a.mu.Unlock()
		return usage, billing.ErrNoResetCredit
	}

	credit := available[0]
	if err := client.RedeemReset(ctx, accessToken, credit.TokenID); err != nil {
		return billing.Usage{}, err
	}

	// 成功后立即从本地缓存移除，避免上游短暂最终一致时重复显示或再次兑换。
	remaining := make([]billing.ResetCredit, 0, len(credits)-1)
	for _, candidate := range credits {
		if candidate.TokenID != credit.TokenID {
			remaining = append(remaining, candidate)
		}
	}
	a.mu.Lock()
	a.BillingUsage.ResetCredits = remaining
	a.BillingUsage.ResetCreditsUpdatedAt = time.Now()
	a.mu.Unlock()

	if refreshed, refreshErr := client.Fetch(ctx, accessToken); refreshErr == nil {
		if refreshed.ResetCreditsUpdatedAt.IsZero() {
			refreshed.ResetCredits = remaining
			refreshed.ResetCreditsUpdatedAt = time.Now()
		} else {
			filtered := refreshed.ResetCredits[:0]
			for _, candidate := range refreshed.ResetCredits {
				if candidate.TokenID != credit.TokenID {
					filtered = append(filtered, candidate)
				}
			}
			refreshed.ResetCredits = filtered
		}
		a.mu.Lock()
		mergeBillingUsage(a, refreshed)
		a.mu.Unlock()
	}

	a.mu.Lock()
	usage := cloneBillingUsage(a.BillingUsage)
	a.mu.Unlock()
	return usage, nil
}

func (p *Pool) refreshBillingAsync(id int64) {
	p.mu.Lock()
	enabled := p.billing != nil
	p.mu.Unlock()
	if !enabled {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := p.refreshBillingAccount(ctx, id); err != nil {
			log.Printf("刷新账号 %d Grok 周用量失败: %v", id, err)
		}
	}()
}

func (p *Pool) refreshBillingAccount(ctx context.Context, id int64) error {
	p.mu.Lock()
	a, ok := p.byID[id]
	client := p.billing
	p.mu.Unlock()
	if !ok || client == nil {
		return nil
	}
	accessToken, err := p.Token(ctx, a)
	if err != nil {
		return err
	}
	usage, err := client.Fetch(ctx, accessToken)
	if err != nil {
		return err
	}
	a.mu.Lock()
	mergeBillingUsage(a, usage)
	a.mu.Unlock()
	return nil
}

func mergeBillingUsage(a *Account, usage billing.Usage) {
	if usage.SubscriptionTier == "" {
		usage.SubscriptionTier = a.BillingUsage.SubscriptionTier
	}
	if usage.ResetCreditsUpdatedAt.IsZero() {
		usage.ResetCredits = append([]billing.ResetCredit(nil), a.BillingUsage.ResetCredits...)
		usage.ResetCreditsUpdatedAt = a.BillingUsage.ResetCreditsUpdatedAt
	} else {
		usage.ResetCredits = append([]billing.ResetCredit(nil), usage.ResetCredits...)
	}
	a.BillingUsage = usage
}

func cloneBillingUsage(usage billing.Usage) billing.Usage {
	usage.ResetCredits = append([]billing.ResetCredit(nil), usage.ResetCredits...)
	return usage
}
