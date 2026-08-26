package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"grok2api/server/internal/api"
	"grok2api/server/internal/auth"
	"grok2api/server/internal/billing"
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

func withV1Prefix(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1" && !strings.HasPrefix(r.URL.Path, "/v1/") {
			if strings.HasPrefix(r.URL.Path, "/") {
				r.URL.Path = "/v1" + r.URL.Path
			} else {
				r.URL.Path = "/v1/" + r.URL.Path
			}
			r.URL.RawPath = ""
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
	p.SetBillingClient(billing.New(cfg.XAIChatProxyBase, cfg.XAIGrokWebBase))
	if err := p.Reload(ctx); err != nil {
		log.Fatalf("加载账号池失败: %v", err)
	}

	kc := auth.NewKeyCache(st)
	if err := kc.Reload(ctx); err != nil {
		log.Fatalf("加载 API Key 失败: %v", err)
	}

	// 启动后立即获取真实订阅周用量，之后每 5 分钟更新一次。
	go func() {
		refresh := func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			p.RefreshBilling(ctx)
		}
		refresh()
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			refresh()
		}
	}()

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

	// 每小时确保未来 7 天的调用记录日分区存在。
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := st.EnsureCallLogPartitions(ctx, time.Now())
			cancel()
			if err != nil {
				log.Printf("创建调用记录分区失败: %v", err)
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

	r.Handle("/api/open/openai/*", requireKeyMW(http.StripPrefix("/api/open/openai", withV1Prefix(openaiV1))))
	r.Handle("/api/open/anthropic/*", requireKeyMW(http.StripPrefix("/api/open/anthropic", withV1Prefix(anthropicV1))))

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
			g.Post("/accounts/{id}/redeem-reset", h.RedeemAccountReset)
			g.Delete("/accounts/{id}", h.DeleteAccount)
			g.Get("/keys", h.ListKeys)
			g.Post("/keys", h.CreateKey)
			g.Delete("/keys/{id}", h.DeleteKey)
			g.Get("/logs", h.ListLogs)
			g.Get("/dashboard", h.Dashboard)
		})
	})

	serveWeb(r, cfg.WebDist)

	addr := net.JoinHostPort(cfg.ListenHost, cfg.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- srv.ListenAndServe()
	}()

	log.Printf("grok2api 启动于 %s（账号池 %d 个账号）", addr, p.Len())
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("服务启动失败: %v", err)
		}
	case <-signalCtx.Done():
		log.Printf("收到停止信号，等待请求完成")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("优雅停止失败: %v", err)
			_ = srv.Close()
		}
		if err := <-serverErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("服务停止异常: %v", err)
		}
	}
}
