package api

import (
	"net/http/httptest"
	"testing"
)

func TestParseLogPagination(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		query      string
		wantLimit  int
		wantOffset int
	}{
		{name: "defaults", wantLimit: 50, wantOffset: 0},
		{name: "valid", query: "?limit=100&offset=200", wantLimit: 100, wantOffset: 200},
		{name: "maximum limit", query: "?limit=1000", wantLimit: 1000, wantOffset: 0},
		{name: "limit too large", query: "?limit=1001", wantLimit: 50, wantOffset: 0},
		{name: "non-positive", query: "?limit=0&offset=-1", wantLimit: 50, wantOffset: 0},
		{name: "invalid", query: "?limit=nope&offset=nope", wantLimit: 50, wantOffset: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/logs"+tt.query, nil)
			limit, offset := parseLogPagination(r)
			if limit != tt.wantLimit || offset != tt.wantOffset {
				t.Fatalf("got limit=%d offset=%d, want limit=%d offset=%d", limit, offset, tt.wantLimit, tt.wantOffset)
			}
		})
	}
}
