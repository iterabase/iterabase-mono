// Package metrics defines bounded Prometheus metrics for the inference gateway.
// Metrics use an isolated registry so tests and serving processes never share
// mutable global collectors.
package metrics

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics holds all Prometheus collectors for the gateway.
type Metrics struct {
	RequestsTotal                           *prometheus.CounterVec
	RequestDuration                         *prometheus.HistogramVec
	TimeToFirstToken                        *prometheus.HistogramVec
	InterTokenLatency                       *prometheus.HistogramVec
	TokensPerSecond                         *prometheus.HistogramVec
	TokensPerRequest                        *prometheus.HistogramVec
	CompletionTokensPerSecondByPromptBucket *prometheus.HistogramVec
	PromptTokensTotal                       *prometheus.CounterVec
	CompletionTokensTotal                   *prometheus.CounterVec
	ActiveStreams                           *prometheus.GaugeVec
	BackendHealth                           *prometheus.GaugeVec
	RateLimitHitsTotal                      *prometheus.CounterVec
	BackendRequestDuration                  *prometheus.HistogramVec
	UpstreamErrorsTotal                     *prometheus.CounterVec
	SnapshotFresh                           prometheus.Gauge
	SnapshotLastRefresh                     prometheus.Gauge
	Registry                                *prometheus.Registry
}

// New creates a development/test registry.
func New(reg *prometheus.Registry) *Metrics { return NewWithBuild(reg, "dev", "none") }

// NewWithBuild creates an isolated production registry with Go/process and
// immutable build information.
func NewWithBuild(reg *prometheus.Registry, version, commit string) *Metrics {
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	build := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "inference_gateway_build_info", Help: "Immutable inference-gateway build information.",
	}, []string{"version", "commit"})
	reg.MustRegister(build)
	build.WithLabelValues(nonempty(version, "dev"), nonempty(commit, "none")).Set(1)

	m := &Metrics{
		Registry: reg,
		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_requests_total", Help: "Total requests processed by the gateway.",
		}, []string{"listener", "route", "model", "status_class", "streaming"}),
		RequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gateway_request_duration_seconds", Help: "Total request duration including streaming.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
		}, []string{"listener", "route", "model", "streaming"}),
		TimeToFirstToken: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gateway_time_to_first_token_seconds", Help: "Time from request to first content chunk (TTFT).",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"model"}),
		InterTokenLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gateway_inter_token_latency_seconds", Help: "Time between consecutive content chunks (ITL).",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
		}, []string{"model"}),
		TokensPerSecond: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gateway_tokens_per_second", Help: "Tokens generated per second per request.",
			Buckets: []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000},
		}, []string{"model", "type"}),
		TokensPerRequest: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gateway_tokens_per_request", Help: "Token counts per request.",
			Buckets: []float64{1, 8, 16, 32, 64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536, 131072},
		}, []string{"model", "type"}),
		CompletionTokensPerSecondByPromptBucket: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gateway_completion_tokens_per_second_by_prompt_bucket", Help: "Completion tokens per second grouped by prompt token bucket.",
			Buckets: []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000},
		}, []string{"model", "prompt_tokens_bucket"}),
		PromptTokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_prompt_tokens_total", Help: "Total prompt tokens processed.",
		}, []string{"model"}),
		CompletionTokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_completion_tokens_total", Help: "Total completion tokens generated.",
		}, []string{"model"}),
		ActiveStreams: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gateway_active_streams", Help: "Currently active streaming connections.",
		}, []string{"model"}),
		BackendHealth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gateway_backend_health", Help: "Snapshot backend availability: 1=available, 0=unavailable.",
		}, []string{"model", "backend_ref"}),
		RateLimitHitsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_rate_limit_hits_total", Help: "Total rate limit rejections.",
		}, []string{"limit_type"}),
		BackendRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gateway_backend_request_duration_seconds", Help: "Backend response time.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
		}, []string{"model", "backend_ref"}),
		UpstreamErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_upstream_errors_total", Help: "Classified model provider failures by error code.",
		}, []string{"model", "code"}),
		SnapshotFresh: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "inference_gateway_snapshot_fresh", Help: "Whether the consumed control-plane snapshot is within the readiness staleness bound.",
		}),
		SnapshotLastRefresh: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "inference_gateway_snapshot_last_refresh_timestamp_seconds", Help: "Unix timestamp of the last successful control-plane snapshot refresh.",
		}),
	}
	reg.MustRegister(
		m.RequestsTotal, m.RequestDuration, m.TimeToFirstToken, m.InterTokenLatency,
		m.TokensPerSecond, m.TokensPerRequest, m.CompletionTokensPerSecondByPromptBucket,
		m.PromptTokensTotal, m.CompletionTokensTotal, m.ActiveStreams, m.BackendHealth,
		m.RateLimitHitsTotal, m.BackendRequestDuration, m.UpstreamErrorsTotal, m.SnapshotFresh, m.SnapshotLastRefresh,
	)
	return m
}

// RegisterDatabasePool exports pgx in-memory statistics without querying Postgres.
func (m *Metrics) RegisterDatabasePool(pool *pgxpool.Pool) {
	if pool == nil {
		return
	}
	m.Registry.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "inference_gateway_database_pool_connections", Help: "Current pgx pool connections."}, func() float64 { return float64(pool.Stat().TotalConns()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "inference_gateway_database_pool_connections_in_use", Help: "Acquired pgx pool connections."}, func() float64 { return float64(pool.Stat().AcquiredConns()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "inference_gateway_database_pool_max_connections", Help: "Configured maximum pgx pool connections."}, func() float64 { return float64(pool.Stat().MaxConns()) }),
	)
}

func nonempty(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
