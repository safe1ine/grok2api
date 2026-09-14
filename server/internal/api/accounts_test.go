package api

import (
	"testing"
	"time"

	"grok2api/server/internal/pool"
)

func TestAccountGroupName(t *testing.T) {
	if name, ok := accountGroupName(" 生产账号 "); !ok || name != "生产账号" {
		t.Fatalf("accountGroupName returned %q, %t", name, ok)
	}
	if _, ok := accountGroupName("   "); ok {
		t.Fatal("blank group name should be invalid")
	}
	if _, ok := accountGroupName(string(make([]rune, 51))); ok {
		t.Fatal("group name longer than 50 characters should be invalid")
	}
}

func TestValidSchedulingWeight(t *testing.T) {
	for _, tc := range []struct {
		weight int
		valid  bool
	}{
		{weight: 0, valid: false},
		{weight: 1, valid: true},
		{weight: 1000, valid: true},
		{weight: 1001, valid: false},
	} {
		if got := validSchedulingWeight(tc.weight); got != tc.valid {
			t.Fatalf("validSchedulingWeight(%d) = %t, want %t", tc.weight, got, tc.valid)
		}
	}
}

func TestApplyAccountStateUsesSchedulerStatus(t *testing.T) {
	until := time.Now().Add(time.Hour)
	view := accountView{}
	view.Status = "active"
	applyAccountState(&view, pool.AccountState{
		Status:        pool.StatusExhausted,
		CooldownUntil: &until,
	})
	if view.Status != pool.StatusExhausted || view.CooldownUntil == nil || !view.CooldownUntil.Equal(until) {
		t.Fatalf("view = %+v", view)
	}
}
