package pool

import (
	"container/heap"
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"grok2api/server/internal/oauth"
	"grok2api/server/internal/store"
)

var (
	ErrNoAccount      = errors.New("没有可用账号")
	ErrAllCoolingDown = errors.New("所有账号都在冷却中")
)

type Account struct {
	ID            int64
	Email         string
	Status        string
	RefreshToken  string // 解密后的明文
	AccessToken   string
	ExpiresAt     time.Time
	CooldownUntil time.Time

	// 最近一次请求的限流信息（内存态，随每次请求更新）
	RLLimit          int
	RLRemaining      int
	RLTokenLimit     int
	RLTokenRemaining int

	mu sync.Mutex

	// checkedOut 表示账号正在被某个请求独占使用（已从堆弹出，尚未归还）。
	// 仅由 p.mu 保护，rebuildHeap 时据此避免把在途账号重复入堆。
	checkedOut bool
}

// accountHeap 按 CooldownUntil 的最小堆。
type accountHeap []*Account

func (h accountHeap) Len() int           { return len(h) }
func (h accountHeap) Less(i, j int) bool { return h[i].CooldownUntil.Before(h[j].CooldownUntil) }
func (h accountHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *accountHeap) Push(x any)        { *h = append(*h, x.(*Account)) }
func (h *accountHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

type Pool struct {
	store *store.Store
	oauth *oauth.Client

	mu   sync.Mutex
	byID map[int64]*Account
	heap accountHeap
}

func New(s *store.Store, oc *oauth.Client) *Pool {
	return &Pool{store: s, oauth: oc, byID: map[int64]*Account{}, heap: accountHeap{}}
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
	p.heap = accountHeap{}
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
		heap.Push(&p.heap, a)
	}
	return nil
}

// AddAccount 新增账号；若 id 已存在（重新授权）则原地更新 refresh_token。
func (p *Pool) AddAccount(id int64, email, refreshToken string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if a, ok := p.byID[id]; ok {
		a.mu.Lock()
		a.RefreshToken = refreshToken
		a.Email = email
		a.Status = "active"
		a.AccessToken = "" // 旧 access_token 作废
		a.mu.Unlock()
		return
	}

	a := &Account{
		ID:            id,
		Email:         email,
		Status:        "active",
		RefreshToken:  refreshToken,
		CooldownUntil: time.Now(),
	}
	p.byID[id] = a
	heap.Push(&p.heap, a)
}

// Acquire 取出一个空闲且未冷却的账号（每账号独占）。
func (p *Pool) Acquire() (*Account, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.heap.Len() == 0 {
		return nil, ErrNoAccount
	}
	top := p.heap[0]
	if top.CooldownUntil.After(time.Now()) {
		return nil, ErrAllCoolingDown
	}
	a := heap.Pop(&p.heap).(*Account)
	a.checkedOut = true
	return a, nil
}

// Release 归还账号；cooldownUntil 为下次可用的最早时间。
func (p *Pool) Release(a *Account, cooldownUntil time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.byID[a.ID]; !ok {
		return // 已被删除
	}
	if !a.checkedOut {
		return // 防重复归还
	}
	a.checkedOut = false
	now := time.Now()
	if cooldownUntil.After(now) {
		a.CooldownUntil = cooldownUntil
	} else {
		a.CooldownUntil = now
	}
	heap.Push(&p.heap, a)
}

// Remove 从池中移除账号（管理员删除）。
func (p *Pool) Remove(id int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.byID, id)
	p.rebuildHeap()
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

func (p *Pool) rebuildHeap() {
	p.heap = accountHeap{}
	for _, a := range p.byID {
		if !a.checkedOut {
			heap.Push(&p.heap, a)
		}
	}
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

// UpdateRateLimit 更新账号最近一次请求的限流信息。
func (p *Pool) UpdateRateLimit(id int64, limit, remaining, tokenLimit, tokenRemaining int) {
	p.mu.Lock()
	a, ok := p.byID[id]
	p.mu.Unlock()
	if !ok {
		return
	}
	a.mu.Lock()
	a.RLLimit = limit
	a.RLRemaining = remaining
	a.RLTokenLimit = tokenLimit
	a.RLTokenRemaining = tokenRemaining
	a.mu.Unlock()
}

// Usage 返回账号最近一次请求的限流信息。
func (p *Pool) Usage(id int64) (limit, remaining, tokenLimit, tokenRemaining int, ok bool) {
	p.mu.Lock()
	a, ok := p.byID[id]
	p.mu.Unlock()
	if !ok {
		return 0, 0, 0, 0, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.RLLimit, a.RLRemaining, a.RLTokenLimit, a.RLTokenRemaining, true
}
