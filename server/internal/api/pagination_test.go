package api

import (
	"net/http/httptest"
	"testing"
	"time"

	"grok2api/server/internal/store"
)

func TestParseLogPagination(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		query     string
		wantLimit int
		wantErr   bool
	}{
		{name: "defaults", wantLimit: 50},
		{name: "valid limit", query: "?limit=100", wantLimit: 100},
		{name: "maximum limit", query: "?limit=1000", wantLimit: 1000},
		{name: "limit too large", query: "?limit=1001", wantLimit: 50},
		{name: "non-positive", query: "?limit=0", wantLimit: 50},
		{name: "invalid limit", query: "?limit=nope", wantLimit: 50},
		{name: "invalid cursor", query: "?cursor=nope", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/logs"+tt.query, nil)
			limit, cursor, err := parseLogPagination(r)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %t", err, tt.wantErr)
			}
			if !tt.wantErr && (limit != tt.wantLimit || cursor != nil) {
				t.Fatalf("got limit=%d cursor=%v, want limit=%d without cursor", limit, cursor, tt.wantLimit)
			}
		})
	}
}

func TestLogCursorRoundTrip(t *testing.T) {
	t.Parallel()
	createdAt := time.Date(2026, 9, 7, 12, 34, 56, 123, time.UTC)
	encoded := encodeLogCursor(store.CallLog{ID: 42, CreatedAt: createdAt})
	r := httptest.NewRequest("GET", "/api/logs?cursor="+encoded, nil)
	limit, cursor, err := parseLogPagination(r)
	if err != nil {
		t.Fatal(err)
	}
	if limit != 50 || cursor == nil || cursor.ID != 42 || !cursor.CreatedAt.Equal(createdAt) {
		t.Fatalf("limit=%d cursor=%+v", limit, cursor)
	}
}
