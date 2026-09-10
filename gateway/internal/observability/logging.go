package observability

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// RequestLog is the one structured log line emitted per gateway request.
// Every field here is required by design (see docs/ARCHITECTURE.md) --
// this is the audit trail an operator uses to answer "why was this
// request allowed/rejected, and what did it cost."
type RequestLog struct {
	Time           time.Time `json:"time"`
	TraceID        string    `json:"trace_id"`
	TenantID       string    `json:"tenant_id"`
	AgentID        string    `json:"agent_id,omitempty"`
	TaskID         string    `json:"task_id,omitempty"`
	Model          string    `json:"model"`
	Provider       string    `json:"provider"`
	TokensIn       int       `json:"tokens_in"`
	TokensOut      int       `json:"tokens_out"`
	CostUSD        float64   `json:"cost_usd"`
	Decision       string    `json:"decision"`        // "allowed" | "rejected" | "advised_retry"
	DecisionReason string    `json:"decision_reason"` // "" on allow; over_tenant_budget / over_agent_budget / over_task_budget / over_rate_limit / upstream_error / ...
	LatencyMs      int64     `json:"latency_ms"`
	StatusCode     int       `json:"status_code"`
}

// AsyncLogger buffers RequestLog entries through a bounded channel and
// writes them as JSON lines on a dedicated goroutine, so a slow stdout
// consumer (a log-shipping sidecar backpressuring, a full pipe buffer on
// a busy host) can never add latency to the request path that produced
// the log line. On overflow, entries are dropped and counted -- silently
// losing a log line under extreme load is preferable to blocking the
// request that generated it.
type AsyncLogger struct {
	ch      chan RequestLog
	out     io.Writer
	logger  *slog.Logger
	dropped atomic.Uint64
}

func NewAsyncLogger(bufferSize int) *AsyncLogger {
	return NewAsyncLoggerWithWriter(bufferSize, os.Stdout)
}

// NewAsyncLoggerWithWriter is the same as NewAsyncLogger but writes JSON
// lines to w instead of stdout -- used by the gateway's integration
// tests to assert on emitted log content (see proxy/handler_test.go).
func NewAsyncLoggerWithWriter(bufferSize int, w io.Writer) *AsyncLogger {
	return &AsyncLogger{
		ch:     make(chan RequestLog, bufferSize),
		out:    w,
		logger: slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{})),
	}
}

// Run drains the buffer and writes each entry as one JSON line to stdout,
// until ctx is cancelled (at which point it drains whatever remains
// synchronously, so a graceful shutdown doesn't lose in-flight logs).
func (al *AsyncLogger) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			al.drain()
			return
		case entry := <-al.ch:
			al.write(entry)
		}
	}
}

func (al *AsyncLogger) drain() {
	for {
		select {
		case entry := <-al.ch:
			al.write(entry)
		default:
			return
		}
	}
}

// Drain synchronously writes every currently-buffered entry without
// requiring Run's background goroutine -- used by tests that want
// deterministic, synchronous log assertions (see proxy/handler_test.go).
func (al *AsyncLogger) Drain() {
	al.drain()
}

func (al *AsyncLogger) write(entry RequestLog) {
	b, err := json.Marshal(entry)
	if err != nil {
		al.logger.Error("failed to marshal request log", "error", err)
		return
	}
	al.out.Write(append(b, '\n'))
}

// Log enqueues entry for async writing. Never blocks: a full buffer
// drops the oldest-pressure entry (i.e. this one) rather than stalling
// the caller, which is always a request-serving goroutine.
func (al *AsyncLogger) Log(entry RequestLog) {
	if entry.Time.IsZero() {
		entry.Time = time.Now().UTC()
	}
	select {
	case al.ch <- entry:
	default:
		al.dropped.Add(1)
	}
}

// DroppedCount reports how many log entries have been dropped due to
// buffer overflow since startup, for surfacing as a metric/health signal.
func (al *AsyncLogger) DroppedCount() uint64 {
	return al.dropped.Load()
}
