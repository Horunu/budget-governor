package proxy

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// SpendEvent mirrors one row of the `spend_events` table (see
// migrations/0001_init.sql). It is the system of record the control
// plane's spend-query endpoints and the reconciliation job both read.
type SpendEvent struct {
	TenantID       string
	AgentID        string
	TaskID         string
	Model          string
	TokensIn       int
	TokensOut      int
	CostUSD        float64
	TraceID        string
	Decision       string
	DecisionReason string
	CreatedAt      time.Time
}

// SpendWriter buffers SpendEvents in a bounded channel and flushes them
// to Postgres in batches on a dedicated goroutine, per the gateway's
// hard "no synchronous DB write on the request path" constraint (see
// docs/ARCHITECTURE.md#hot-path-latency-budget). Enqueue never blocks:
// on a full buffer, the event is dropped and counted via the
// spend_write_dropped_total metric rather than backpressuring the
// request that generated it.
type SpendWriter struct {
	sink          SpendSink
	ch            chan SpendEvent
	flushInterval time.Duration
	batchSize     int
	logger        *slog.Logger
	dropped       prometheus.Counter
}

func NewSpendWriter(sink SpendSink, queueCapacity, batchSize int, flushInterval time.Duration, logger *slog.Logger, dropped prometheus.Counter) *SpendWriter {
	return &SpendWriter{
		sink:          sink,
		ch:            make(chan SpendEvent, queueCapacity),
		flushInterval: flushInterval,
		batchSize:     batchSize,
		logger:        logger,
		dropped:       dropped,
	}
}

// Enqueue never blocks the caller.
func (w *SpendWriter) Enqueue(e SpendEvent) {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	select {
	case w.ch <- e:
	default:
		w.dropped.Inc()
		w.logger.Warn("spend event dropped: write buffer full", "tenant_id", e.TenantID, "trace_id", e.TraceID)
	}
}

// Run drains the buffer, flushing every flushInterval or once batchSize
// events have accumulated, whichever comes first, until ctx is
// cancelled -- at which point it performs one final flush so a graceful
// shutdown doesn't lose buffered events.
func (w *SpendWriter) Run(ctx context.Context) {
	batch := make([]SpendEvent, 0, w.batchSize)
	ticker := time.NewTicker(w.flushInterval)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := w.sink.WriteBatch(context.Background(), batch); err != nil {
			w.logger.Error("spend batch write failed", "batch_size", len(batch), "error", err)
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-ctx.Done():
			w.drainRemaining(&batch)
			flush()
			return
		case e := <-w.ch:
			batch = append(batch, e)
			if len(batch) >= w.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (w *SpendWriter) drainRemaining(batch *[]SpendEvent) {
	for {
		select {
		case e := <-w.ch:
			*batch = append(*batch, e)
		default:
			return
		}
	}
}

