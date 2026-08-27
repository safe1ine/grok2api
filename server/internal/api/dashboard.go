package api

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"grok2api/server/internal/store"
)

const (
	dashboardTimezone     = "Asia/Shanghai"
	pricingSource         = "https://docs.x.ai/developers/models"
	pricingSnapshotDate   = "2026-08-24"
	longContextThreshold  = int64(200000)
	defaultDashboardRange = 360
	maxDashboardRange     = 7 * 24 * 60
)

type modelPricing struct {
	Model               string  `json:"model"`
	CanonicalModel      string  `json:"canonical_model"`
	InputUSDPerMillion  float64 `json:"input_usd_per_million"`
	CachedUSDPerMillion float64 `json:"cached_usd_per_million"`
	OutputUSDPerMillion float64 `json:"output_usd_per_million"`
	LongInputUSD        float64 `json:"long_input_usd_per_million"`
	LongCachedUSD       float64 `json:"long_cached_usd_per_million"`
	LongOutputUSD       float64 `json:"long_output_usd_per_million"`
	LongThreshold       int64   `json:"long_context_threshold"`
}

type dashboardPoint struct {
	Timestamp    string  `json:"timestamp"`
	Calls        int64   `json:"calls"`
	InputTokens  int64   `json:"input_tokens"`
	CachedTokens int64   `json:"cached_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

type dashboardTotals struct {
	Calls        int64   `json:"calls"`
	InputTokens  int64   `json:"input_tokens"`
	CachedTokens int64   `json:"cached_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

type dashboardResponse struct {
	RangeMinutes   int                    `json:"range_minutes"`
	Timezone       string                 `json:"timezone"`
	From           string                 `json:"from"`
	To             string                 `json:"to"`
	Points         []dashboardPoint       `json:"points"`
	Totals         dashboardTotals        `json:"totals"`
	Models         []string               `json:"models"`
	Keys           []store.UsageKeyOption `json:"keys"`
	Pricing        []modelPricing         `json:"pricing"`
	UnpricedModels []string               `json:"unpriced_models"`
	PricingSource  string                 `json:"pricing_source"`
	PricingAsOf    string                 `json:"pricing_as_of"`
}

var officialModelPricing = buildOfficialModelPricing()

func buildOfficialModelPricing() map[string]modelPricing {
	prices := make(map[string]modelPricing)
	add := func(canonical string, input, cached, output, longInput, longCached, longOutput float64, aliases ...string) {
		base := modelPricing{
			CanonicalModel: canonical, InputUSDPerMillion: input, CachedUSDPerMillion: cached,
			OutputUSDPerMillion: output, LongInputUSD: longInput, LongCachedUSD: longCached,
			LongOutputUSD: longOutput, LongThreshold: longContextThreshold,
		}
		for _, model := range append([]string{canonical}, aliases...) {
			price := base
			price.Model = model
			prices[model] = price
		}
	}

	add("grok-4.6", 2, 0.5, 6, 4, 1, 12)
	add("grok-4.5", 2, 0.3, 6, 4, 0.6, 12, "grok-4.5-latest", "grok-build-latest")
	add("grok-4.3", 1.25, 0.2, 2.5, 2.5, 0.4, 5, "grok-4.3-latest")
	add("grok-4.20-0309-reasoning", 1.25, 0.2, 2.5, 2.5, 0.4, 5,
		"grok-4.20", "grok-4.20-0309", "grok-4.20-reasoning", "grok-4.20-reasoning-latest",
		"grok-4.20-beta", "grok-4.20-beta-0309", "grok-4.20-beta-0309-reasoning",
		"grok-4.20-beta-latest", "grok-4.20-beta-latest-reasoning", "grok-4.20-beta-reasoning",
		"grok-4.20-experimental-beta-0304", "grok-4.20-experimental-beta-0304-reasoning",
		"grok-4.20-experimental-beta-latest", "grok-4.20-experimental-beta-reasoning-latest",
		"grok-4.20-reasoning-gv2")
	add("grok-4.20-0309-non-reasoning", 1.25, 0.2, 2.5, 2.5, 0.4, 5,
		"grok-4.20-non-reasoning", "grok-4.20-non-reasoning-latest",
		"grok-4.20-beta-non-reasoning", "grok-4.20-beta-latest-non-reasoning",
		"grok-4.20-beta-0309-non-reasoning", "grok-4.20-experimental-beta-0304-non-reasoning",
		"grok-4.20-experimental-beta-non-reasoning-latest", "grok-4.20-non-reasoning-gv2")
	add("grok-4.20-multi-agent-0309", 1.25, 0.2, 2.5, 2.5, 0.4, 5,
		"grok-4.20-multi-agent", "grok-4.20-multi-agent-latest", "grok-4.20-multi-agent-beta-0309",
		"grok-4.20-multi-agent-beta-latest", "grok-4.20-multi-agent-experimental-beta-0304",
		"grok-4.20-multi-agent-experimental-beta-latest")
	add("grok-build-0.1", 1, 0.2, 2, 2, 0.4, 4,
		"grok-code-fast-1", "grok-code-fast", "grok-code-fast-1-0825")
	return prices
}

func estimateUsageCost(usage store.MinuteUsage) (float64, modelPricing, bool) {
	price, ok := officialModelPricing[usage.Model]
	if !ok {
		return 0, modelPricing{}, false
	}

	input := max(int64(0), usage.PromptTokens)
	cached := min(input, max(int64(0), usage.CachedTokens))
	output := max(int64(0), usage.CompletionTokens)
	longInput := min(input, max(int64(0), usage.LongContextPromptTokens))
	longCached := min(longInput, cached, max(int64(0), usage.LongContextCachedTokens))
	longOutput := min(output, max(int64(0), usage.LongContextCompletionTokens))

	standardInput := input - longInput
	standardCached := min(standardInput, cached-longCached)
	standardOutput := output - longOutput
	standardUncached := standardInput - standardCached
	longUncached := longInput - longCached

	cost := (float64(standardUncached)*price.InputUSDPerMillion +
		float64(standardCached)*price.CachedUSDPerMillion +
		float64(standardOutput)*price.OutputUSDPerMillion +
		float64(longUncached)*price.LongInputUSD +
		float64(longCached)*price.LongCachedUSD +
		float64(longOutput)*price.LongOutputUSD) / 1_000_000
	return cost, price, true
}

type dashboardFilters struct {
	Model string
	KeyID *int64
}

func parseDashboardFilters(r *http.Request) dashboardFilters {
	filters := dashboardFilters{Model: strings.TrimSpace(r.URL.Query().Get("model"))}
	if value := r.URL.Query().Get("key_id"); value != "" {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil && parsed > 0 {
			filters.KeyID = &parsed
		}
	}
	return filters
}

func parseDashboardRange(r *http.Request) int {
	minutes := defaultDashboardRange
	if value := r.URL.Query().Get("minutes"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed >= 1 && parsed <= maxDashboardRange {
			minutes = parsed
		}
	}
	return minutes
}

func (h *Handler) Dashboard(w http.ResponseWriter, r *http.Request) {
	loc, err := time.LoadLocation(dashboardTimezone)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "加载统计时区失败")
		return
	}
	minutes := parseDashboardRange(r)
	filters := parseDashboardFilters(r)
	end := time.Now().In(loc).Truncate(time.Minute).Add(time.Minute)
	start := end.Add(-time.Duration(minutes) * time.Minute)

	usage, err := h.store.ListMinuteUsage(r.Context(), start, end, filters.Model, filters.KeyID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	models, err := h.store.ListUsageModels(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	keys, err := h.store.ListUsageKeyOptions(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	points := make([]dashboardPoint, minutes)
	for index := range points {
		points[index].Timestamp = start.Add(time.Duration(index) * time.Minute).Format(time.RFC3339)
	}
	priced := make(map[string]modelPricing)
	unpriced := make(map[string]struct{})
	for _, row := range usage {
		index := int(row.Minute.Sub(start) / time.Minute)
		if index < 0 || index >= len(points) {
			continue
		}
		point := &points[index]
		point.Calls += row.Calls
		point.InputTokens += row.PromptTokens
		point.CachedTokens += row.CachedTokens
		point.OutputTokens += row.CompletionTokens
		if cost, price, ok := estimateUsageCost(row); ok {
			point.CostUSD += cost
			priced[price.CanonicalModel] = price
		} else if row.Model != "" {
			unpriced[row.Model] = struct{}{}
		}
	}

	response := dashboardResponse{
		RangeMinutes:   minutes,
		Timezone:       dashboardTimezone,
		From:           start.Format(time.RFC3339),
		To:             end.Format(time.RFC3339),
		Points:         points,
		Models:         models,
		Keys:           keys,
		Pricing:        make([]modelPricing, 0),
		UnpricedModels: make([]string, 0),
		PricingSource:  pricingSource,
		PricingAsOf:    pricingSnapshotDate,
	}
	for _, point := range points {
		response.Totals.Calls += point.Calls
		response.Totals.InputTokens += point.InputTokens
		response.Totals.CachedTokens += point.CachedTokens
		response.Totals.OutputTokens += point.OutputTokens
		response.Totals.CostUSD += point.CostUSD
	}
	for _, price := range priced {
		price.Model = price.CanonicalModel
		response.Pricing = append(response.Pricing, price)
	}
	sort.Slice(response.Pricing, func(i, j int) bool { return response.Pricing[i].Model < response.Pricing[j].Model })
	for model := range unpriced {
		response.UnpricedModels = append(response.UnpricedModels, model)
	}
	sort.Strings(response.UnpricedModels)

	writeJSON(w, http.StatusOK, response)
}
