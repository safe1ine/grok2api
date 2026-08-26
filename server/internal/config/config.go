package config

import (
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	Port        string
	ListenHost  string
	DatabaseURL string

	// xAI OAuth
	XAIAuthBase      string
	XAIAPIBase       string
	XAIChatProxyBase string
	XAIGrokWebBase   string
	XAIClientID      string
	XAIClientSecret  string // 公开客户端 + PKCE 时可为空
	XAIScope         string
	XAIRedirectURI   string

	// 管理台
	AdminUsername string
	AdminPassword string
	JWTSecret     []byte

	// 是否要求下游携带 API Key
	RequireAuth bool

	// refresh_token 落库加密密钥（内部 sha256 派生为 32 字节）
	EncryptionKey []byte

	// 可选：前端构建产物目录
	WebDist string
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func Load() (*Config, error) {
	// 自动加载当前目录的 .env（已存在的环境变量优先，不覆盖）
	_ = godotenv.Load()

	cfg := &Config{
		Port:             getenv("PORT", "30081"),
		ListenHost:       os.Getenv("LISTEN_HOST"),
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		XAIAuthBase:      strings.TrimRight(getenv("XAI_AUTH_BASE", "https://auth.x.ai"), "/"),
		XAIAPIBase:       strings.TrimRight(getenv("XAI_API_BASE", "https://api.x.ai"), "/"),
		XAIChatProxyBase: strings.TrimRight(getenv("XAI_CHAT_PROXY_BASE", "https://cli-chat-proxy.grok.com/v1"), "/"),
		XAIGrokWebBase:   strings.TrimRight(getenv("XAI_GROK_WEB_BASE", "https://grok.com"), "/"),
		XAIClientID:      getenv("XAI_CLIENT_ID", "b1a00492-073a-47ea-816f-4c329264a828"),
		XAIClientSecret:  os.Getenv("XAI_CLIENT_SECRET"),
		XAIScope:         getenv("XAI_SCOPE", "openid profile email offline_access grok-cli:access api:access"),
		XAIRedirectURI:   getenv("XAI_REDIRECT_URI", "http://localhost:30081/api/oauth/callback"),
		AdminUsername:    getenv("ADMIN_USERNAME", "admin"),
		AdminPassword:    os.Getenv("ADMIN_PASSWORD"),
		RequireAuth:      parseBool(getenv("REQUIRE_AUTH", "true")),
		WebDist:          os.Getenv("WEB_DIST"),
	}

	var missing []string
	if cfg.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if cfg.AdminPassword == "" {
		missing = append(missing, "ADMIN_PASSWORD")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("缺少环境变量: %s", strings.Join(missing, ", "))
	}

	// JWT 密钥：优先 JWT_SECRET，否则用 ADMIN_PASSWORD 派生
	jwtRaw := getenv("JWT_SECRET", cfg.AdminPassword)
	sum := sha256.Sum256([]byte(jwtRaw))
	cfg.JWTSecret = sum[:]

	// 加密密钥：优先 ENCRYPTION_KEY，否则用 ADMIN_PASSWORD 派生
	encRaw := getenv("ENCRYPTION_KEY", cfg.AdminPassword)
	encSum := sha256.Sum256([]byte(encRaw))
	cfg.EncryptionKey = encSum[:]

	if os.Getenv("ENCRYPTION_KEY") == "" {
		log.Printf("[警告] 未设置 ENCRYPTION_KEY，refresh_token 将用 ADMIN_PASSWORD 派生密钥加密；" +
			"若日后修改 ADMIN_PASSWORD，已登录的账号将无法解密，需要重新授权。建议显式设置 ENCRYPTION_KEY。")
	}

	return cfg, nil
}
