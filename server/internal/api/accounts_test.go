package api

import (
	"testing"
	"time"

	"grok2api/server/internal/billing"
)

func TestApplyAccountUsageReportsAvailableResetCreditsWithoutTokenIDs(t *testing.T) {
	now := time.Now()
	soon := now.Add(48 * time.Hour)
	usage := billing.Usage{
		SubscriptionTier:      "SuperGrok Heavy",
		WeeklyUsedPercent:     15,
		WeeklyResetAt:         now.Add(7 * 24 * time.Hour),
		ResetCreditsUpdatedAt: now,
		ResetCredits: []billing.ResetCredit{
			{TokenID: "expired-secret", ValidFrom: now.Add(-48 * time.Hour), ExpiresAt: now.Add(-time.Hour)},
			{TokenID: "available-secret", ValidFrom: now.Add(-time.Hour), ExpiresAt: soon},
		},
	}
	var view accountView
	applyAccountUsage(&view, usage)

	if !view.ResetCreditsKnown || view.ResetCreditsAvailable != 1 {
		t.Fatalf("view = %+v", view)
	}
	if view.ResetCreditExpiresAt == nil || !view.ResetCreditExpiresAt.Equal(soon) {
		t.Fatalf("expires at = %v, want %v", view.ResetCreditExpiresAt, soon)
	}
}

func TestApplyAccountUsageKeepsFailedResetLookupUnknown(t *testing.T) {
	var view accountView
	applyAccountUsage(&view, billing.Usage{WeeklyUsedPercent: 10, UpdatedAt: time.Now()})
	if view.ResetCreditsKnown || view.ResetCreditsAvailable != 0 || view.ResetCreditExpiresAt != nil {
		t.Fatalf("view = %+v", view)
	}
}
