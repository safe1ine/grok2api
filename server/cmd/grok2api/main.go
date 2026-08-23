package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/go-chi/chi/v5"

	"grok2api/server/internal/api"
	"grok2api/server/internal/auth"
	"grok2api/server/internal/config"
	"grok2api/server/internal/gateway"
	"grok2api/server/internal/oauth"
	"grok2api/server/internal/pool"
	"grok2api/server/internal/store"
)

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Api-Key, X-Admin-Token")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func serveWeb(r chi.Router, dist string) {
	if dist == "" {
		return
	}
	if _, err := os.Stat(dist); err != nil {
		return
	}
	fs := http.FileServer(http.Dir(dist))
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		p := filepath.Join(dist, req.URL.Path)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			fs.ServeHTTP(w, req)
			return
		}
		http.ServeFile(w, req, filepath.Join(dist, "index.html"))
	})
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("配置错误: %v", err)
	}

	ctx := context.Background()
	st, err := store.New(ctx, cfg.DatabaseURL, cfg.EncryptionKey)
	if err != nil {
		log.Fatalf("初始化存储失败: %v", err)
	}
	defer st.Close()

	oc := oauth.New(cfg.XAIAuthBase, cfg.XAIClientID, cfg.XAIClientSecret, cfg.XAIRedirectURI, cfg.XAIScope)
	p := pool.New(st, oc)
	if err := p.Reload(ctx); err != nil {
		log.Fatalf("加载账号池失败: %v", err)
	}

	kc := auth.NewKeyCache(st)
	if err := kc.Reload(ctx); err != nil {
		log.Fatalf("加载 API Key 失败: %v", err)
	}

	// 后台定期清理过期的 OAuth 授权状态
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			if n, err := st.CleanupExpiredOAuthStates(context.Background()); err != nil {
				log.Printf("清理过期 oauth_states 失败: %v", err)
			} else if n > 0 {
				log.Printf("清理过期 oauth_states %d 条", n)
			}
		}
	}()

	gw := gateway.New(cfg, p, st)
	h := api.New(cfg, st, p, oc, kc)

	requireKeyMW := auth.RequireKey(kc, cfg.RequireAuth)
	requireAdminMW := auth.RequireAdmin(cfg)

	// OpenAI 前缀：/api/open/openai/v1/*
	openaiV1 := chi.NewRouter()
	openaiV1.Get("/v1/models", gw.HandleModels)
	openaiV1.Post("/v1/audio/speech", gw.HandleTTS)
	openaiV1.Post("/v1/audio/transcriptions", gw.HandleSTT)
	openaiV1.HandleFunc("/*", gw.Proxy)

	// Anthropic 前缀：/api/open/anthropic/v1/*
	anthropicV1 := chi.NewRouter()
	anthropicV1.Get("/v1/models", gw.HandleModels)
	anthropicV1.HandleFunc("/*", gw.Proxy)

	r := chi.NewRouter()
	r.Use(corsMiddleware)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"accounts":%d}`, p.Len())
	})

	r.Handle("/api/open/openai/*", requireKeyMW(http.StripPrefix("/api/open/openai", openaiV1)))
	r.Handle("/api/open/anthropic/*", requireKeyMW(http.StripPrefix("/api/open/anthropic", anthropicV1)))

	r.Route("/api", func(rt chi.Router) {
		rt.Post("/auth/login", h.Login)
		rt.Get("/oauth/callback", h.OAuthCallback)

		rt.Group(func(g chi.Router) {
			g.Use(requireAdminMW)
			g.Post("/oauth/authorize-url", h.OAuthAuthorizeURL)
			g.Post("/oauth/complete", h.OAuthComplete)
			g.Post("/oauth/device", h.DeviceStart)
			g.Get("/oauth/device/status", h.DeviceStatus)
			g.Get("/accounts", h.ListAccounts)
			g.Delete("/accounts/{id}", h.DeleteAccount)
			g.Get("/keys", h.ListKeys)
			g.Post("/keys", h.CreateKey)
			g.Delete("/keys/{id}", h.DeleteKey)
			g.Get("/logs", h.ListLogs)
		})
	})

	serveWeb(r, cfg.WebDist)

	log.Printf("grok2api 启动于 :%s（账号池 %d 个账号）", cfg.Port, p.Len())
	if err := http.ListenAndServe(":"+cfg.Port, r); err != nil {
		log.Fatalf("服务启动失败: %v", err)
	}
}
