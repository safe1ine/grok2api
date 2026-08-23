package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"grok2api/server/internal/config"
	"grok2api/server/internal/store"
)

// ---------- 下游 API Key ----------

func HashKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// KeyCache 内存缓存 key_hash -> keyID，避免每次请求查库。
type KeyCache struct {
	mu     sync.RWMutex
	hashes map[string]int64
	store  *store.Store
}

func NewKeyCache(s *store.Store) *KeyCache {
	return &KeyCache{hashes: map[string]int64{}, store: s}
}

func (k *KeyCache) Reload(ctx context.Context) error {
	m, err := k.store.ListActiveKeyHashes(ctx)
	if err != nil {
		return err
	}
	k.mu.Lock()
	k.hashes = m
	k.mu.Unlock()
	return nil
}

func (k *KeyCache) Lookup(hash string) (int64, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	id, ok := k.hashes[hash]
	return id, ok
}

// ExtractKey 从 Authorization Bearer 或 x-api-key 取明文 key。
func ExtractKey(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	if k := r.Header.Get("x-api-key"); k != "" {
		return strings.TrimSpace(k)
	}
	return ""
}

// RequireKey 下游鉴权中间件。
func RequireKey(kc *KeyCache, require bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !require {
				next.ServeHTTP(w, r)
				return
			}
			key := ExtractKey(r)
			if key == "" {
				http.Error(w, `{"error":"missing api key"}`, http.StatusUnauthorized)
				return
			}
			id, ok := kc.Lookup(HashKey(key))
			if !ok {
				http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
				return
			}
			// 把 keyID 放进 context，供记录调用日志用
			ctx := context.WithValue(r.Context(), ctxKeyID{}, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

type ctxKeyID struct{}

func KeyIDFromContext(r *http.Request) (int64, bool) {
	id, ok := r.Context().Value(ctxKeyID{}).(int64)
	return id, ok
}

// ---------- 管理台 JWT ----------

type AdminClaims struct {
	Username string `json:"username"`
	jwt.RegisteredClaims
}

func IssueAdminToken(cfg *config.Config, username string, ttl time.Duration) (string, error) {
	claims := AdminClaims{
		Username: username,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "grok2api",
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(cfg.JWTSecret)
}

func VerifyAdminToken(cfg *config.Config, tokenStr string) (*AdminClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &AdminClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return cfg.JWTSecret, nil
	})
	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(*AdminClaims)
	if !ok || !token.Valid {
		return nil, errors.New("invalid token")
	}
	return claims, nil
}

// RequireAdmin 管理端鉴权中间件。
func RequireAdmin(cfg *config.Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(strings.ToLower(auth), "bearer ") {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			if _, err := VerifyAdminToken(cfg, strings.TrimSpace(auth[7:])); err != nil {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
