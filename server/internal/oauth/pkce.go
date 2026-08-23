package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// NewVerifier 生成 PKCE code_verifier（43~128 字符）。
func NewVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Challenge 由 verifier 计算 S256 code_challenge。
func Challenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}
