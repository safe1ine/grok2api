package pool

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"grok2api/server/internal/billing"
)

func TestAcquireAllowsConcurrentSingleAccount(t *testing.T) {
	p := New(nil, nil)
	p.AddAccount(1, "a@x.com", "rt1")

	a1, err := p.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	a2, err := p.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if a1 != a2 || a1.ID != 1 {
		t.Fatal("单账号并发请求应复用同一账号")
	}
	if a1.inFlight != 2 {
		t.Fatalf("inFlight = %d, want 2", a1.inFlight)
	}

	p.Release(a1, time.Now())
	p.Release(a2, time.Now())
	if a1.inFlight != 0 {
		t.Fatalf("inFlight = %d, want 0", a1.inFlight)
	}
}

func TestAcquirePrefersLeastInFlightAndRotatesTies(t *testing.T) {
	p := New(nil, nil)
	p.AddAccount(1, "a@x.com", "rt1")
	p.AddAccount(2, "b@x.com", "rt2")

	a1, err := p.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	a2, err := p.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if a1.ID == a2.ID {
		t.Fatal("第二次请求应优先选择零并发账号")
	}

	a3, err := p.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if a3.ID != a1.ID {
		t.Fatalf("并发相同时应选择最久未分配账号，得到 %d，期望 %d", a3.ID, a1.ID)
	}

	p.Release(a1, time.Now())
	p.Release(a2, time.Now())
	p.Release(a3, time.Now())
}

func TestConcurrentReleaseDoesNotClearCooldown(t *testing.T) {
	p := New(nil, nil)
	p.AddAccount(1, "a@x.com", "rt1")

	a1, _ := p.Acquire()
	a2, _ := p.Acquire()
	cooldownUntil := time.Now().Add(time.Minute)
	p.Release(a1, cooldownUntil)
	p.Release(a2, time.Now())

	if _, err := p.Acquire(); !errors.Is(err, ErrAllCoolingDown) {
		t.Fatalf("Acquire error = %v, want ErrAllCoolingDown", err)
	}
	if !a1.CooldownUntil.Equal(cooldownUntil) {
		t.Fatalf("cooldown = %v, want %v", a1.CooldownUntil, cooldownUntil)
	}

	p.mu.Lock()
	a1.CooldownUntil = time.Now().Add(-time.Second)
	p.mu.Unlock()
	a3, err := p.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	p.Release(a3, time.Now())
}

func TestAcquireBalancesConcurrentLoad(t *testing.T) {
	p := New(nil, nil)
	const accountCount = 20
	const leasesPerAccount = 5
	for i := 0; i < accountCount; i++ {
		p.AddAccount(int64(i+1), "a", "rt")
	}

	leases := make([]*Account, 0, accountCount*leasesPerAccount)
	counts := map[int64]int{}
	for i := 0; i < accountCount*leasesPerAccount; i++ {
		a, err := p.Acquire()
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, a)
		counts[a.ID]++
	}
	for id, count := range counts {
		if count != leasesPerAccount {
			t.Fatalf("账号 %d 分配 %d 次，期望 %d", id, count, leasesPerAccount)
		}
	}
	for _, a := range leases {
		p.Release(a, time.Now())
	}
}

func TestRefreshBillingCachesLatestSuccessfulUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/billing":
			_, _ = w.Write([]byte(`{"config":{"creditUsagePercent":15,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-08-31T08:12:21Z"}}}`))
		case "/settings":
			_, _ = w.Write([]byte(`{"subscription_tier_display":"SuperGrok Heavy"}`))
		default:
			http.NotFound(w, r)
		}
	}))

	p := New(nil, nil)
	p.AddAccount(1, "a@x.com", "refresh")
	p.byID[1].AccessToken = "access"
	p.byID[1].ExpiresAt = time.Now().Add(time.Hour)
	p.SetBillingClient(billing.New(server.URL, server.URL))
	p.RefreshBilling(context.Background())

	usage, ok := p.BillingUsage(1)
	if !ok || usage.SubscriptionTier != "SuperGrok Heavy" || usage.WeeklyUsedPercent != 15 {
		t.Fatalf("usage = %+v, ok = %t", usage, ok)
	}
	server.Close()
	p.RefreshBilling(context.Background())
	preserved, ok := p.BillingUsage(1)
	if !ok || !reflect.DeepEqual(preserved, usage) {
		t.Fatalf("failed refresh replaced usage: before=%+v after=%+v", usage, preserved)
	}
}

func TestRefreshBillingPreservesResetCreditsWhenSupplementalLookupFails(t *testing.T) {
	var failResets atomic.Bool
	now := time.Now().UTC().Truncate(time.Second)
	resetResponse := poolResetResponse(map[string]time.Time{"reset-token": now.Add(48 * time.Hour)})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/billing":
			_, _ = w.Write([]byte(`{"config":{"creditUsagePercent":15,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-08-31T08:12:21Z"}}}`))
		case "/settings":
			_, _ = w.Write([]byte(`{"subscription_tier_display":"SuperGrok Heavy"}`))
		case "/prod_mc_billing.ConsumerUiSvc/GetRemainingResets":
			if failResets.Load() {
				http.Error(w, "temporary", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write(resetResponse)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	p := New(nil, nil)
	p.AddAccount(1, "a@x.com", "refresh")
	p.byID[1].AccessToken = "access"
	p.byID[1].ExpiresAt = time.Now().Add(time.Hour)
	p.SetBillingClient(billing.New(server.URL, server.URL))
	p.RefreshBilling(context.Background())
	before, _ := p.BillingUsage(1)
	if len(before.AvailableResetCredits(now)) != 1 || before.ResetCreditsUpdatedAt.IsZero() {
		t.Fatalf("before = %+v", before)
	}

	failResets.Store(true)
	p.RefreshBilling(context.Background())
	after, _ := p.BillingUsage(1)
	if !reflect.DeepEqual(after.ResetCredits, before.ResetCredits) || !after.ResetCreditsUpdatedAt.Equal(before.ResetCreditsUpdatedAt) {
		t.Fatalf("reset credits were replaced: before=%+v after=%+v", before, after)
	}
}

func TestRedeemResetUsesSoonestCreditAndRefreshesUsage(t *testing.T) {
	var redeemed atomic.Bool
	now := time.Now().UTC().Truncate(time.Second)
	resetResponse := poolResetResponse(map[string]time.Time{
		"later-token": now.Add(5 * 24 * time.Hour),
		"soon-token":  now.Add(2 * 24 * time.Hour),
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/billing":
			used := 15
			if redeemed.Load() {
				used = 0
			}
			_, _ = w.Write([]byte(`{"config":{"creditUsagePercent":` + strconv.Itoa(used) + `,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-08-31T08:12:21Z"}}}`))
		case "/settings":
			_, _ = w.Write([]byte(`{"subscription_tier_display":"SuperGrok Heavy"}`))
		case "/prod_mc_billing.ConsumerUiSvc/GetRemainingResets":
			if redeemed.Load() {
				_, _ = w.Write(poolGRPCResponse(nil))
			} else {
				_, _ = w.Write(resetResponse)
			}
		case "/prod_mc_billing.ConsumerUiSvc/RedeemReset":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "soon-token") || strings.Contains(string(body), "later-token") {
				t.Errorf("redeem body = %x", body)
			}
			redeemed.Store(true)
			w.Header().Set("grpc-status", "0")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	p := New(nil, nil)
	p.AddAccount(1, "a@x.com", "refresh")
	p.byID[1].AccessToken = "access"
	p.byID[1].ExpiresAt = time.Now().Add(time.Hour)
	p.SetBillingClient(billing.New(server.URL, server.URL))
	p.RefreshBilling(context.Background())

	before, ok := p.BillingUsage(1)
	if !ok || len(before.AvailableResetCredits(now)) != 2 {
		t.Fatalf("before = %+v, ok = %t", before, ok)
	}
	usage, err := p.RedeemReset(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !redeemed.Load() || usage.WeeklyUsedPercent != 0 || len(usage.AvailableResetCredits(time.Now())) != 0 {
		t.Fatalf("usage after redeem = %+v, redeemed = %t", usage, redeemed.Load())
	}
}

func TestConcurrentAcquireSameAccount(t *testing.T) {
	p := New(nil, nil)
	p.AddAccount(1, "a", "rt")

	const requestCount = 200
	var wg sync.WaitGroup
	errs := make(chan error, requestCount)
	for i := 0; i < requestCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := p.Acquire()
			if err != nil {
				errs <- err
				return
			}
			time.Sleep(time.Millisecond)
			p.Release(a, time.Now())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := p.byID[1].inFlight; got != 0 {
		t.Fatalf("inFlight = %d, want 0", got)
	}
}

func TestRemoveAccountWithInFlightRequest(t *testing.T) {
	p := New(nil, nil)
	p.AddAccount(1, "a", "rt")
	a, _ := p.Acquire()
	p.Remove(1)

	if _, err := p.Acquire(); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("Acquire error = %v, want ErrNoAccount", err)
	}
	p.Release(a, time.Now()) // 已删除账号的在途请求结束时应安全忽略。
}

func poolResetResponse(credits map[string]time.Time) []byte {
	var root []byte
	for id, expiresAt := range credits {
		var token []byte
		token = poolProtoBytes(token, 10, []byte(id))
		timestamp := poolVarint(nil, 1<<3)
		timestamp = poolVarint(timestamp, uint64(expiresAt.Unix()))
		token = poolProtoBytes(token, 30, timestamp)
		root = poolProtoBytes(root, 10, token)
	}
	return poolGRPCResponse(root)
}

func poolGRPCResponse(message []byte) []byte {
	data := make([]byte, 5, 5+len(message))
	data[1] = byte(len(message) >> 24)
	data[2] = byte(len(message) >> 16)
	data[3] = byte(len(message) >> 8)
	data[4] = byte(len(message))
	data = append(data, message...)
	trailer := []byte("grpc-status: 0\r\n")
	frame := make([]byte, 5, 5+len(trailer))
	frame[0] = 0x80
	frame[4] = byte(len(trailer))
	frame = append(frame, trailer...)
	return append(data, frame...)
}

func poolProtoBytes(dst []byte, field int, value []byte) []byte {
	dst = poolVarint(dst, uint64(field<<3|2))
	dst = poolVarint(dst, uint64(len(value)))
	return append(dst, value...)
}

func poolVarint(dst []byte, value uint64) []byte {
	for value >= 0x80 {
		dst = append(dst, byte(value&0x7f)|0x80)
		value >>= 7
	}
	return append(dst, byte(value))
}
