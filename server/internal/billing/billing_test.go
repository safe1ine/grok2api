package billing

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchWeeklyUsageAndTier(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/prod_mc_billing.ConsumerUiSvc/GetRemainingResets" {
			_, _ = w.Write(append(grpcFrame(nil), grpcTrailer(0)...))
			return
		}
		if r.Header.Get("Authorization") != "Bearer access-token" || r.Header.Get("X-XAI-Token-Auth") != "xai-grok-cli" {
			t.Fatalf("auth headers = %#v", r.Header)
		}
		if r.Header.Get("x-grok-client-version") != clientVersion {
			t.Fatalf("client version = %q", r.Header.Get("x-grok-client-version"))
		}
		switch r.URL.RequestURI() {
		case "/billing?format=credits":
			if r.Header.Get("x-grok-client-mode") != "billing" {
				t.Fatalf("client mode = %q", r.Header.Get("x-grok-client-mode"))
			}
			_, _ = w.Write([]byte(`{"config":{"creditUsagePercent":15,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-08-24T08:12:21Z","end":"2026-08-31T08:12:21Z"}}}`))
		case "/settings":
			_, _ = w.Write([]byte(`{"subscription_tier_display":"SuperGrok Heavy"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	usage, err := New(server.URL, server.URL).Fetch(context.Background(), "access-token")
	if err != nil {
		t.Fatal(err)
	}
	if usage.SubscriptionTier != "SuperGrok Heavy" || usage.WeeklyUsedPercent != 15 {
		t.Fatalf("usage = %+v", usage)
	}
	wantReset := time.Date(2026, 8, 31, 8, 12, 21, 0, time.UTC)
	if !usage.WeeklyResetAt.Equal(wantReset) || usage.UpdatedAt.IsZero() {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestFetchUsesBillingPeriodEndAndKeepsUsageWhenSettingsFails(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/settings" {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"config":{"creditUsagePercent":120,"billingPeriodEnd":"2026-08-28T07:24:06Z","currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY"}}}`))
	}))
	defer server.Close()

	usage, err := New(server.URL, server.URL).Fetch(context.Background(), "access-token")
	if err != nil {
		t.Fatal(err)
	}
	if usage.WeeklyUsedPercent != 100 || usage.SubscriptionTier != "" {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestFetchTreatsMissingWeeklyUsageAsZero(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		config string
	}{
		{name: "missing", config: `"isUnifiedBillingUser":true,`},
		{name: "null", config: `"creditUsagePercent":null,"isUnifiedBillingUser":true,`},
		{name: "legacy response without unified flag", config: ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, `{"config":{%s"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-08-31T08:12:21Z"}}}`, tc.config)
			}))
			defer server.Close()

			usage, err := New(server.URL, server.URL).Fetch(context.Background(), "access-token")
			if err != nil {
				t.Fatal(err)
			}
			if usage.WeeklyUsedPercent != 0 {
				t.Fatalf("usage = %+v, want 0%%", usage)
			}
		})
	}
}

func TestFetchRejectsMissingUsageForExplicitNonUnifiedBilling(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"config":{"isUnifiedBillingUser":false,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-08-31T08:12:21Z"}}}`))
	}))
	defer server.Close()

	if _, err := New(server.URL, server.URL).Fetch(context.Background(), "access-token"); err == nil {
		t.Fatal("expected missing usage error for non-unified billing")
	}
}

func TestFetchRejectsNonWeeklyPeriod(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"config":{"creditUsagePercent":10,"billingPeriodEnd":"2026-08-28T07:24:06Z","currentPeriod":{"type":"USAGE_PERIOD_TYPE_MONTHLY"}}}`))
	}))
	defer server.Close()

	if _, err := New(server.URL, server.URL).Fetch(context.Background(), "access-token"); err == nil {
		t.Fatal("expected non-weekly period error")
	}
}

func resetCreditsMessage(credits ...ResetCredit) []byte {
	var root []byte
	for _, credit := range credits {
		var token []byte
		token = appendProtoBytes(token, 10, []byte(credit.TokenID))
		if !credit.ValidFrom.IsZero() {
			token = appendProtoBytes(token, 20, protoTimestamp(credit.ValidFrom))
		}
		if !credit.ExpiresAt.IsZero() {
			token = appendProtoBytes(token, 30, protoTimestamp(credit.ExpiresAt))
		}
		root = appendProtoBytes(root, 10, token)
	}
	return root
}

func protoTimestamp(value time.Time) []byte {
	message := appendVarint(nil, 1<<3)
	return appendVarint(message, uint64(value.Unix()))
}

func grpcTrailer(status int) []byte {
	payload := []byte(fmt.Sprintf("grpc-status: %d\r\n", status))
	frame := grpcFrame(payload)
	frame[0] = 0x80
	return frame
}
