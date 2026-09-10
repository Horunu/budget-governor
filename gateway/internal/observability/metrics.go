// Package observability implements the gateway's two non-negotiable
// signals: Prometheus metrics (pulled) and structured JSON request logs
// (written to stdout). Both are designed to add zero synchronous
// network I/O to the request path -- Prometheus counters/histograms are
// in-memory atomic operations scraped on Prometheus's own schedule, and
// logs are written through a buffered handler.
package observability

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics bundles every Prometheus collector the gateway registers.
// Label cardinality is kept intentionally bounded: tenant_id is included
// where it drives the "Tenant Overview" dashboard, but never combined
// with unbounded labels like trace_id.
type Metrics struct {
	RequestsTotal       *prometheus.CounterVec
	RequestDuration     *prometheus.HistogramVec
	TokensTotal         *prometheus.CounterVec
	CostUSDTotal        *prometheus.CounterVec
	RejectionsTotal     *prometheus.CounterVec
	BucketSaturation    *prometheus.GaugeVec
	AdvisorInvocations  *prometheus.CounterVec
	SpendWriteDropped   prometheus.Counter
	ProviderErrorsTotal *prometheus.CounterVec
}

// NewMetrics constructs and registers all collectors against reg. Passing
// a dedicated (non-default) registry keeps gateway metrics isolated from
// any process-level collectors added by dependencies, which matters for
// the alert rules in observability/prometheus/alerts.yml staying exact.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "budget_governor",
			Subsystem: "gateway",
			Name:      "requests_total",
			Help:      "Total LLM call requests handled, by tenant, model, and final decision.",
		}, []string{"tenant_id", "model", "decision"}),

		RequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "budget_governor",
			Subsystem: "gateway",
			Name:      "request_duration_seconds",
			Help:      "End-to-end gateway request latency, including upstream provider time.",
			// Buckets span sub-millisecond gateway overhead up to
			// multi-second upstream generation time, so both the
			// p99-overhead claim and real-world tail latency are visible
			// from the same histogram.
			Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"tenant_id", "model", "decision"}),

		TokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "budget_governor",
			Subsystem: "gateway",
			Name:      "tokens_total",
			Help:      "Total tokens consumed, by tenant, model, and direction (in/out).",
		}, []string{"tenant_id", "model", "direction"}),

		CostUSDTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "budget_governor",
			Subsystem: "gateway",
			Name:      "cost_usd_total",
			Help:      "Total gateway-observed cost in USD, by tenant and model.",
		}, []string{"tenant_id", "model"}),

		RejectionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "budget_governor",
			Subsystem: "gateway",
			Name:      "rejections_total",
			Help:      "Requests rejected by the budget enforcer, by tenant and reason.",
		}, []string{"tenant_id", "reason"}),

		BucketSaturation: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "budget_governor",
			Subsystem: "gateway",
			Name:      "bucket_saturation_ratio",
			Help:      "Fraction of the token budget consumed (0-1) for the most-constrained bucket on the last request, by tenant and scope.",
		}, []string{"tenant_id", "scope"}),

		AdvisorInvocations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "budget_governor",
			Subsystem: "gateway",
			Name:      "advisor_invocations_total",
			Help:      "Times the cost advisor was consulted under budget pressure, by tenant and outcome (suggested/timeout/error).",
		}, []string{"tenant_id", "outcome"}),

		SpendWriteDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "budget_governor",
			Subsystem: "gateway",
			Name:      "spend_write_dropped_total",
			Help:      "Spend events dropped because the async write buffer to Postgres overflowed.",
		}),

		ProviderErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "budget_governor",
			Subsystem: "gateway",
			Name:      "provider_errors_total",
			Help:      "Upstream provider errors, by provider and HTTP status class (e.g. 4xx, 5xx).",
		}, []string{"provider", "status_class"}),
	}

	reg.MustRegister(
		m.RequestsTotal,
		m.RequestDuration,
		m.TokensTotal,
		m.CostUSDTotal,
		m.RejectionsTotal,
		m.BucketSaturation,
		m.AdvisorInvocations,
		m.SpendWriteDropped,
		m.ProviderErrorsTotal,
	)
	return m
}
