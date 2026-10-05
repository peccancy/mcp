// Command mcp serves disputes.online to AI agents over the Model Context
// Protocol: a public, read-only window onto open disputes and how the stakes
// on them are split.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/time/rate"

	"github.com/peccancy/mcp/internal/platform"
	"github.com/peccancy/mcp/internal/tools"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	port := env("APP_PORT", "8098")
	// Mounted under a path so the ingress can forward /mcp as it is, without a
	// strip-prefix middleware.
	path := "/" + strings.Trim(env("API_PATH", "/mcp"), "/")

	client := platform.New(
		env("DISPUTES_URL", "http://disputes:8080"),
		env("CATEGORIES_URL", "http://categories:8082"),
		env("HISTORY_URL", "http://history:8095"),
		envDuration(log, "PLATFORM_TIMEOUT", 5*time.Second),
	)

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "disputes-online",
		Title:   "disputes.online",
		Version: env("APP_VERSION", "dev"),
	}, &mcp.ServerOptions{Instructions: tools.Instructions})
	tools.Register(server, client, log)

	// Stateless: every tool is a plain read, so there is nothing to keep
	// between requests and any replica can answer any of them.
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})

	// The server is open to anyone, so the only brake is on the total rate. It
	// is sized far above honest use and well below what the dispute service
	// would notice.
	limiter := rate.NewLimiter(rate.Limit(envInt(log, "RATE_LIMIT_RPS", 20)), envInt(log, "RATE_LIMIT_BURST", 40))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	limited := http.MaxBytesHandler(rateLimit(limiter, handler), 1<<20)
	mux.Handle(path, limited)
	mux.Handle(path+"/", limited)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("mcp server listening", "port", port, "path", path)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		log.Error("shutdown failed", "error", err)
	}
}

func rateLimit(limiter *rate.Limiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !limiter.Allow() {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(log *slog.Logger, key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		log.Warn("ignoring invalid setting", "key", key, "value", v)
		return fallback
	}
	return n
}

func envDuration(log *slog.Logger, key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Warn("ignoring invalid setting", "key", key, "value", v)
		return fallback
	}
	return d
}
