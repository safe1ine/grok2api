package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWithV1Prefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
		want string
	}{
		{name: "already versioned", url: "/v1/chat/completions", want: "/v1/chat/completions"},
		{name: "unversioned", url: "/chat/completions", want: "/v1/chat/completions"},
		{name: "models", url: "/models", want: "/v1/models"},
		{name: "version root", url: "/v1", want: "/v1"},
		{name: "root", url: "/", want: "/v1/"},
		{name: "query preserved", url: "/responses?stream=true", want: "/v1/responses?stream=true"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			h := withV1Prefix(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = r.URL.RequestURI()
			}))
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, tt.url, nil))
			if got != tt.want {
				t.Fatalf("path = %q, want %q", got, tt.want)
			}
		})
	}
}
