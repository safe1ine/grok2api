package api

import (
	"math"
	"net/http/httptest"
	"testing"

	"grok2api/server/internal/store"
)

func TestEstimateUsageCostGrok46WithCachedInput(t *testing.T) {
	t.Parallel()

	usage := store.MinuteUsage{
		Model: "grok-4.6", PromptTokens: 100, CachedTokens: 40, CompletionTokens: 10,
	}
	cost, price, ok := estimateUsageCost(usage)
	if !ok {
		t.Fatal("grok-4.6 should have official pricing")
	}
	// (60 × $2 + 40 × $0.50 + 10 × $6) / 1M = $0.0002.
	if math.Abs(cost-0.0002) > 1e-12 {
		t.Fatalf("cost = %.12f, want 0.0002", cost)
	}
	if price.CanonicalModel != "grok-4.6" {
		t.Fatalf("canonical model = %q", price.CanonicalModel)
	}
}

func TestEstimateUsageCostUsesLongContextRates(t *testing.T) {
	t.Parallel()

	usage := store.MinuteUsage{
		Model: "grok-4.6", LongContext: true, PromptTokens: 300000,
		CachedTokens: 200000, CompletionTokens: 10000,
	}
	cost, _, ok := estimateUsageCost(usage)
	if !ok {
		t.Fatal("grok-4.6 should have official pricing")
	}
	// 100k × $4 + 200k × $1 + 10k × $12 = $0.72.
	if math.Abs(cost-0.72) > 1e-12 {
		t.Fatalf("cost = %.12f, want 0.72", cost)
	}
}

func TestEstimateUsageCostRecognizesOfficialAlias(t *testing.T) {
	t.Parallel()

	usage := store.MinuteUsage{Model: "grok-4.20", PromptTokens: 1_000_000}
	cost, price, ok := estimateUsageCost(usage)
	if !ok {
		t.Fatal("official alias should have pricing")
	}
	if cost != 1.25 || price.CanonicalModel != "grok-4.20-0309-reasoning" {
		t.Fatalf("cost=%v canonical=%q", cost, price.CanonicalModel)
	}
}

func TestEstimateUsageCostDoesNotGuessUnknownModel(t *testing.T) {
	t.Parallel()

	cost, _, ok := estimateUsageCost(store.MinuteUsage{Model: "unknown", PromptTokens: 1_000_000})
	if ok || cost != 0 {
		t.Fatalf("unknown model got cost=%v ok=%v", cost, ok)
	}
}

func TestParseDashboardRange(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		query string
		want  int
	}{
		{"", defaultDashboardRange},
		{"?minutes=60", 60},
		{"?minutes=10080", 10080},
		{"?minutes=0", defaultDashboardRange},
		{"?minutes=10081", defaultDashboardRange},
		{"?minutes=bad", defaultDashboardRange},
	} {
		r := httptest.NewRequest("GET", "/api/dashboard"+test.query, nil)
		if got := parseDashboardRange(r); got != test.want {
			t.Errorf("query %q: got %d, want %d", test.query, got, test.want)
		}
	}
}
