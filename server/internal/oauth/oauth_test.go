package oauth

import (
	"encoding/base64"
	"testing"
)

func TestSubjectFromIDToken(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"user@example.com","sub":"user-123"}`))
	token := "header." + payload + ".signature"
	if got := SubjectFromIDToken(token); got != "user-123" {
		t.Fatalf("SubjectFromIDToken() = %q", got)
	}
	if got := EmailFromIDToken(token); got != "user@example.com" {
		t.Fatalf("EmailFromIDToken() = %q", got)
	}
}
