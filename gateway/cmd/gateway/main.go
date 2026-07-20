// Command gateway is the budget-governor hot-path service: it
// authenticates requests, enforces per-tenant/agent/task budgets against
// Redis, and proxies to an upstream LLM provider (OpenAI, Anthropic, or
// the offline Mock provider). See docs/ARCHITECTURE.md for the full
// design and docs/RUNBOOK.md for operational guidance.
package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"github.com/horunu/budget-governor/gateway/internal/auth"
	"github.com/horunu/budget-governor/gateway/internal/budget"
	"github.com/horunu/budget-governor/gateway/internal/config"
	"github.com/horunu/budget-governor/gateway/internal/observability"
	"github.com/horunu/budget-governor/gateway/internal/policy"
	"github.com/horunu/budget-governor/gateway/internal/provider"
	"github.com/horunu/budget-governor/gateway/internal/proxy"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{}))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config load failed", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := sql.Open("postgres", cfg.PostgresDSN)
	if err != nil {
		logger.Error("postgres open failed", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	waitForPostgres(ctx, db, logger)

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})
	defer rdb.Close()
	waitForRedis(ctx, rdb, logger)

	enforcer := budget.NewEnforcer(rdb, int(24*time.Hour/time.Second))
	if err := enforcer.LoadScript(ctx); err != nil {
		logger.Error("failed to load budget lua script", "error", err)
		os.Exit(1)
	}

	authStore := auth.NewStore(db, cfg.AuthCacheTTL, logger)
	go authStore.RunRefreshLoop(ctx)

	configCache := budget.NewConfigCache(db, cfg.BudgetCacheTTL, logger)
	if err := configCache.Refresh(ctx); err != nil {
		logger.Error("initial budget config load failed", "error", err)
		os.Exit(1)
	}
	go configCache.RunRefreshLoop(ctx)

	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)

	asyncLogger := observability.NewAsyncLogger(10000)
	go asyncLogger.Run(ctx)

	spendWriter := proxy.NewSpendWriter(db, cfg.SpendQueueCapacity, cfg.SpendFlushBatchSize, cfg.SpendFlushInterval, logger, metrics.SpendWriteDropped)
	go spendWriter.Run(ctx)

	router := proxy.ProviderRouter{
		OpenAI:    provider.NewOpenAIProvider(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL),
		Anthropic: provider.NewAnthropicProvider(cfg.AnthropicAPIKey, cfg.AnthropicBaseURL),
		Mock:      provider.NewMockProvider(cfg.MockProviderLatencyMinMs, cfg.MockProviderLatencyMaxMs),
	}

	handler := &proxy.Handler{
		AuthStore:    authStore,
		ConfigCache:  configCache,
		Enforcer:     enforcer,
		Router:       router,
		Advisor:      proxy.NewAdvisorClient(cfg.AdvisorURL, cfg.AdvisorTimeout),
		PolicyConfig: policy.DefaultConfig(),
		Metrics:      metrics,
		Logger:       asyncLogger,
		SpendWriter:  spendWriter,
		DB:           db,
		Logf:         logger,
	}

	if cfg.Environment == "production" {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/healthz", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/readyz", readinessHandler(db, rdb))
	handler.RegisterRoutes(r)

	apiServer := &http.Server{Addr: cfg.HTTPAddr, Handler: r}

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	metricsServer := &http.Server{Addr: cfg.MetricsAddr, Handler: metricsMux}

	go func() {
		logger.Info("gateway listening", "addr", cfg.HTTPAddr)
		if err := apiServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("gateway server error", "error", err)
		}
	}()
	go func() {
		logger.Info("metrics listening", "addr", cfg.MetricsAddr)
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("metrics server error", "error", err)
		}
	}()

	<-ctx.Done()
	logger.Info("shutdown signal received, draining")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = apiServer.Shutdown(shutdownCtx)
	_ = metricsServer.Shutdown(shutdownCtx)

	// Give the async logger and spend writer a moment to flush after
	// their driving ctx is already cancelled by signal.NotifyContext.
	time.Sleep(500 * time.Millisecond)
	logger.Info("shutdown complete")
}

func readinessHandler(db *sql.DB, rdb *redis.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"ready": false, "reason": "postgres unavailable"})
			return
		}
		if err := rdb.Ping(ctx).Err(); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"ready": false, "reason": "redis unavailable"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ready": true})
	}
}

func waitForPostgres(ctx context.Context, db *sql.DB, logger *slog.Logger) {
	for i := 0; i < 30; i++ {
		if err := db.PingContext(ctx); err == nil {
			return
		}
		logger.Info("waiting for postgres", "attempt", i+1)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func waitForRedis(ctx context.Context, rdb *redis.Client, logger *slog.Logger) {
	for i := 0; i < 30; i++ {
		if err := rdb.Ping(ctx).Err(); err == nil {
			return
		}
		logger.Info("waiting for redis", "attempt", i+1)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}
