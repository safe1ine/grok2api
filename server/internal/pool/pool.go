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

const explicitExhaustionMinHold = 5 * time.Minute

const (
	StatusActive      = "active"
	StatusCooldown    = "cooldown"
	StatusExhausted   = "exhausted"
	StatusNeedRelogin = "need_relogin"
	StatusDisabled    = "disabled"
)

var (
	ErrNoAccount       = errors.New("没有可用账号")
	ErrAllCoolingDown  = errors.New("所有账号都在冷却或额度耗尽状态")
	ErrAccountNotFound = errors.New("账号不存在或未启用")
)

// AccountState 是调度器实际使用的运行状态，同时用于账号列表展示。
type AccountState struct {
	Status        string
	CooldownUntil *time.Time
	InFlight      int
}

type Account struct {
	ID                  int64
	Email               string
	Status              string
	RefreshToken        string // 解密后的明文
	AccessToken         string
	ExpiresAt           time.Time
	CooldownUntil       time.Time
	quotaExhaustedAt    time.Time // 上游明确返回额度耗尽的时间，用于区分之后的新鲜 billing 数据。
	quotaExhaustedUntil time.Time // 上游明确返回额度耗尽后的最长停用时间。
	SchedulingDisabled  bool      // 人工调度开关；不影响在途请求和周用量刷新。
	Weight              int       // 调度权重；值越大，空闲和同负载时获得的请求越多。

	// Grok 订阅周用量（内存缓存，由 billing endpoint 定期刷新）。
	BillingUsage billing.Usage

	mu      sync.Mutex
	resetMu sync.Mutex // 序列化同一账号的重置券兑换，避免重复消费。

	// 调度计数仅由 Pool.mu 保护，用于加权最少连接和历史分配比例。
	inFlight     int
	assignments  uint64
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

// Reload 从数据库全量载入凭据有效的账号，包括人工禁用账号。
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
			ID:                 r.ID,
			Email:              r.Email,
			Status:             r.Status,
			RefreshToken:       r.RefreshToken,
			CooldownUntil:      now,
			SchedulingDisabled: r.SchedulingDisabled,
			Weight:             normalizeWeight(r.SchedulingWeight),
		}
		if a.SchedulingDisabled {
			a.Status = StatusDisabled
		}
		p.byID[r.ID] = a
	}
	return nil
}

// AddAccount 新增默认权重账号；若 id 已存在（重新授权）则保留已有权重。
func (p *Pool) AddAccount(id int64, email, refreshToken string) {
	p.AddAccountWithWeight(id, email, refreshToken, 0)
}

func (p *Pool) AddAccountWithWeight(id int64, email, refreshToken string, weight int) {
	p.mu.Lock()
	if a, ok := p.byID[id]; ok {
		a.mu.Lock()
		a.RefreshToken = refreshToken
		a.Email = email
		a.Status = StatusActive
		a.SchedulingDisabled = false
		if weight > 0 {
			a.Weight = normalizeWeight(weight)
		} else if a.Weight < 1 {
			a.Weight = 1
		}
		a.CooldownUntil = time.Time{}
		a.quotaExhaustedAt = time.Time{}
		a.quotaExhaustedUntil = time.Time{}
		a.AccessToken = "" // 旧 access_token 作废
		a.mu.Unlock()
	} else {
		p.byID[id] = &Account{
			ID:            id,
			Email:         email,
			Status:        StatusActive,
			RefreshToken:  refreshToken,
			CooldownUntil: time.Now(),
			Weight:        normalizeWeight(weight),
		}
	}
	p.mu.Unlock()
	p.RefreshBillingAsync(id)
}

// Acquire 选择一个可用且当前并发最少的账号。
// 账号不会被独占移出池，同一账号可以同时服务多个请求。
func (p *Pool) Acquire() (*Account, error) {
	return p.AcquireExcluding(nil)
}

// AcquireExcluding 为一次请求选择尚未尝试过的账号，避免故障转移又选回同一账号。
func (p *Pool) AcquireExcluding(excluded map[int64]struct{}) (*Account, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.byID) == 0 {
		return nil, ErrNoAccount
	}

	now := time.Now()
	var best *Account
	for _, a := range p.byID {
		p.recalculateTimedStatusLocked(a, now)
		if a.Status != StatusActive {
			continue
		}
		if _, skip := excluded[a.ID]; skip {
			continue
		}
		if best == nil || weightedAccountLess(a, best) {
			best = a
		}
	}
	if best == nil {
		return nil, ErrAllCoolingDown
	}

	p.pickSeq++
	best.inFlight++
	best.assignments++
	best.lastAssigned = p.pickSeq
	return best, nil
}

func normalizeWeight(weight int) int {
	if weight < 1 {
		return 1
	}
	return weight
}

func weightedAccountLess(candidate, current *Account) bool {
	candidateWeight := uint64(normalizeWeight(candidate.Weight))
	currentWeight := uint64(normalizeWeight(current.Weight))
	candidateLoad := uint64(candidate.inFlight) * currentWeight
	currentLoad := uint64(current.inFlight) * candidateWeight
	if candidateLoad != currentLoad {
		return candidateLoad < currentLoad
	}
	candidateShare := candidate.assignments * currentWeight
	currentShare := current.assignments * candidateWeight
	if candidateShare != currentShare {
		return candidateShare < currentShare
	}
	return candidate.lastAssigned < current.lastAssigned
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
	p.recalculateTimedStatusLocked(a, time.Now())
}

// ReleaseQuotaExhausted 结束租用并记录上游明确返回的账号额度耗尽状态。
// billing 百分比可能稍有延迟，因此先保持短暂防抖；之后的新鲜 billing 数据可恢复账号。
func (p *Pool) ReleaseQuotaExhausted(a *Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	current, ok := p.byID[a.ID]
	if !ok || current != a {
		return
	}
	if a.inFlight > 0 {
		a.inFlight--
	}
	now := time.Now()
	until := now.Add(explicitExhaustionMinHold)
	a.mu.Lock()
	if a.BillingUsage.WeeklyResetAt.After(now) {
		until = a.BillingUsage.WeeklyResetAt
	}
	a.mu.Unlock()
	a.quotaExhaustedAt = now
	a.quotaExhaustedUntil = until
	a.CooldownUntil = until
	if a.SchedulingDisabled {
		a.Status = StatusDisabled
	} else {
		a.Status = StatusExhausted
	}
}

// AccountState 返回账号列表与调度器共用的实时状态。
func (p *Pool) AccountState(id int64) (AccountState, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.byID[id]
	if !ok {
		return AccountState{}, false
	}
	p.recalculateTimedStatusLocked(a, time.Now())
	state := AccountState{Status: a.Status, InFlight: a.inFlight}
	if a.Status == StatusCooldown || a.Status == StatusExhausted {
		until := a.CooldownUntil
		state.CooldownUntil = &until
	}
	return state, true
}

// RecalculateStatuses 根据最近一次成功周用量和冷却时间统一校准运行状态。
func (p *Pool) RecalculateStatuses(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.byID {
		p.recalculateAccountStatusLocked(a, now)
	}
}

func explicitQuotaExhaustionActiveLocked(a *Account, now time.Time) bool {
	if a.quotaExhaustedUntil.IsZero() {
		return false
	}
	barrierUntil := a.quotaExhaustedUntil
	if barrierUntil.After(now) {
		a.mu.Lock()
		usage := a.BillingUsage
		a.mu.Unlock()
		freshRecovery := !a.quotaExhaustedAt.IsZero() &&
			!now.Before(a.quotaExhaustedAt.Add(explicitExhaustionMinHold)) &&
			usage.UpdatedAt.After(a.quotaExhaustedAt) &&
			usage.WeeklyUsedPercent < 99
		if !freshRecovery {
			a.Status = StatusExhausted
			a.CooldownUntil = barrierUntil
			return true
		}
	}
	a.quotaExhaustedAt = time.Time{}
	a.quotaExhaustedUntil = time.Time{}
	if !a.CooldownUntil.After(barrierUntil) {
		a.CooldownUntil = time.Time{}
	}
	a.Status = StatusActive
	return false
}

func (p *Pool) recalculateTimedStatusLocked(a *Account, now time.Time) {
	if a.SchedulingDisabled {
		a.Status = StatusDisabled
		return
	}
	if explicitQuotaExhaustionActiveLocked(a, now) {
		return
	}
	if a.Status == StatusExhausted && a.CooldownUntil.After(now) {
		return
	}
	if a.CooldownUntil.After(now) {
		a.Status = StatusCooldown
		return
	}
	a.CooldownUntil = time.Time{}
	a.Status = StatusActive
}

func (p *Pool) recalculateAccountStatusLocked(a *Account, now time.Time) {
	if a.SchedulingDisabled {
		a.Status = StatusDisabled
		return
	}
	if explicitQuotaExhaustionActiveLocked(a, now) {
		return
	}

	a.mu.Lock()
	usage := a.BillingUsage
	a.mu.Unlock()

	wasExhausted := a.Status == StatusExhausted
	if !usage.UpdatedAt.IsZero() && usage.WeeklyUsedPercent >= 99 && usage.WeeklyResetAt.After(now) {
		a.Status = StatusExhausted
		a.CooldownUntil = usage.WeeklyResetAt
		return
	}
	if wasExhausted {
		// 周期已重置，或刷新后确认额度恢复；清除原来的周限截止时间。
		a.CooldownUntil = time.Time{}
	}
	if a.CooldownUntil.After(now) {
		a.Status = StatusCooldown
		return
	}
	a.CooldownUntil = time.Time{}
	a.Status = StatusActive
}

// SetSchedulingDisabled 设置人工调度开关。禁用不影响已经分配的在途请求。
func (p *Pool) SetSchedulingDisabled(id int64, disabled bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.byID[id]
	if !ok {
		return false
	}
	a.SchedulingDisabled = disabled
	p.recalculateAccountStatusLocked(a, time.Now())
	return true
}

// SetWeight 更新账号调度权重，并重置历史分配计数以立即应用新比例。
func (p *Pool) SetWeight(id int64, weight int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.byID[id]
	if !ok {
		return false
	}
	a.Weight = normalizeWeight(weight)
	for _, account := range p.byID {
		account.assignments = 0
	}
	return true
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

// MarkNeedRelogin 标记需要重新授权并移出运行池。
func (p *Pool) MarkNeedRelogin(ctx context.Context, a *Account) {
	if err := p.store.SetAccountStatus(ctx, a.ID, StatusNeedRelogin); err != nil {
		log.Printf("标记账号 %d 需要重新授权失败: %v", a.ID, err)
	}
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

// RefreshBilling 刷新所有账号的订阅等级、周用量和重置时间，并重新计算运行状态。
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
	p.RecalculateStatuses(time.Now())
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
	now = time.Now()
	a.mu.Lock()
	a.BillingUsage.WeeklyUsedPercent = 0
	a.BillingUsage.UpdatedAt = now
	a.BillingUsage.ResetCredits = remaining
	a.BillingUsage.ResetCreditsUpdatedAt = now
	a.mu.Unlock()
	p.mu.Lock()
	if current, ok := p.byID[a.ID]; ok && current == a {
		a.CooldownUntil = time.Time{}
		a.quotaExhaustedAt = time.Time{}
		a.quotaExhaustedUntil = time.Time{}
		p.recalculateAccountStatusLocked(a, time.Now())
	}
	p.mu.Unlock()

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

// RefreshBillingAsync 在 429 或新增账号后立即异步校准该账号的周用量状态。
func (p *Pool) RefreshBillingAsync(id int64) {
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
		if errors.Is(err, oauth.ErrInvalidGrant) {
			p.MarkNeedRelogin(ctx, a)
		}
		return err
	}
	usage, err := client.Fetch(ctx, accessToken)
	if err != nil {
		return err
	}
	if usage.ResetCreditsError != "" {
		log.Printf("刷新账号 %d Grok 重置次数失败: %s", id, usage.ResetCreditsError)
	}
	a.mu.Lock()
	mergeBillingUsage(a, usage)
	a.mu.Unlock()
	p.mu.Lock()
	if current, ok := p.byID[id]; ok && current == a {
		p.recalculateAccountStatusLocked(a, time.Now())
	}
	p.mu.Unlock()
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
