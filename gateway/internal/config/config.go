// Package config loads gateway runtime configuration from environment
// variables. All values have safe local-dev defaults so the gateway can
// boot inside docker-compose with only a handful of required overrides.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	// HTTPAddr is the address the gateway listens on for LLM proxy traffic.
	HTTPAddr string
	// MetricsAddr is the address Prometheus scrapes /metrics from.
	MetricsAddr string

	RedisAddr     string
	RedisPassword string
	RedisDB       int

	PostgresDSN string

	// AdvisorURL is the base URL of the cost advisor service.
	AdvisorURL     string
	AdvisorTimeout time.Duration

	// AuthCacheTTL is how long a validated API key is trusted before the
	// background refresher re-checks Postgres.
	AuthCacheTTL time.Duration
	// BudgetCacheTTL is how long budget config (limits, refill rates) is
	// cached in-memory before being re-read from Postgres.
	BudgetCacheTTL time.Duration

	// SpendFlushInterval and SpendFlushBatchSize bound how long a spend
	// event can sit in the async write buffer before being flushed.
	SpendFlushInterval  time.Duration
	SpendFlushBatchSize int
	SpendQueueCapacity  int

	// MockProviderLatencyMinMs/MaxMs bound the mock provider's simulated
	// upstream latency, used so the demo and load test see realistic
	// tail-latency shapes without calling a real LLM API.
	MockProviderLatencyMinMs int
	MockProviderLatencyMaxMs int

	OpenAIAPIKey     string
	OpenAIBaseURL    string
	AnthropicAPIKey  string
	AnthropicBaseURL string

	Environment string
}

func Load() (*Config, error) {
	cfg := &Config{
		HTTPAddr:                 getEnv("GATEWAY_HTTP_ADDR", ":8080"),
		MetricsAddr:              getEnv("GATEWAY_METRICS_ADDR", ":9090"),
		RedisAddr:                getEnv("REDIS_ADDR", "localhost:6379"),
		RedisPassword:            getEnv("REDIS_PASSWORD", ""),
		RedisDB:                  getEnvInt("REDIS_DB", 0),
		PostgresDSN:              getEnv("POSTGRES_DSN", "postgres://budget_governor:budget_governor@localhost:5432/budget_governor?sslmode=disable"),
		AdvisorURL:               getEnv("ADVISOR_URL", "http://localhost:8082"),
		AdvisorTimeout:           getEnvDuration("ADVISOR_TIMEOUT_MS", 300*time.Millisecond),
		AuthCacheTTL:             getEnvDuration("AUTH_CACHE_TTL_SECONDS", 60*time.Second),
		BudgetCacheTTL:           getEnvDuration("BUDGET_CACHE_TTL_SECONDS", 30*time.Second),
		SpendFlushInterval:       getEnvDuration("SPEND_FLUSH_INTERVAL_MS", 200*time.Millisecond),
		SpendFlushBatchSize:      getEnvInt("SPEND_FLUSH_BATCH_SIZE", 500),
		SpendQueueCapacity:       getEnvInt("SPEND_QUEUE_CAPACITY", 50000),
		MockProviderLatencyMinMs: getEnvInt("MOCK_PROVIDER_LATENCY_MIN_MS", 50),
		MockProviderLatencyMaxMs: getEnvInt("MOCK_PROVIDER_LATENCY_MAX_MS", 500),
		OpenAIAPIKey:             getEnv("OPENAI_API_KEY", ""),
		OpenAIBaseURL:            getEnv("OPENAI_BASE_URL", "https://api.openai.com/v1"),
		AnthropicAPIKey:          getEnv("ANTHROPIC_API_KEY", ""),
		AnthropicBaseURL:         getEnv("ANTHROPIC_BASE_URL", "https://api.anthropic.com/v1"),
		Environment:              getEnv("ENVIRONMENT", "development"),
	}
	if cfg.MockProviderLatencyMinMs > cfg.MockProviderLatencyMaxMs {
		return nil, fmt.Errorf("config: MOCK_PROVIDER_LATENCY_MIN_MS (%d) > MOCK_PROVIDER_LATENCY_MAX_MS (%d)", cfg.MockProviderLatencyMinMs, cfg.MockProviderLatencyMaxMs)
	}
	return cfg, nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// getEnvDuration reads an integer number of milliseconds from the named
// env var (matching the _MS suffix convention used throughout), falling
// back to def (already a time.Duration) when unset. Vars ending in
// _SECONDS are interpreted as whole seconds instead.
func getEnvDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	if len(key) > 8 && key[len(key)-8:] == "_SECONDS" {
		return time.Duration(n) * time.Second
	}
	return time.Duration(n) * time.Millisecond
}
