// Package metrics owns bounded Prometheus instrumentation shared by the
// control-plane API, tool gateway, dispatch server, and manager.
//
// Every serving process uses an isolated registry. Metrics deliberately avoid
// customer, workflow, work-item, run, turn, tool, credential, URL, and error
// values as labels.
package metrics

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics is the complete bounded control-plane collector set. A fresh
// instance is created per process; unobserved vector metrics emit no series.
type Metrics struct {
	Registry *prometheus.Registry

	HTTPRequests      *prometheus.CounterVec
	HTTPDuration      *prometheus.HistogramVec
	HTTPInFlight      *prometheus.GaugeVec
	HTTPResponseBytes *prometheus.CounterVec

	GatewayRunnerConnections   *prometheus.GaugeVec
	GatewayRunnerStreams       *prometheus.CounterVec
	GatewayInvocations         *prometheus.CounterVec
	GatewayInvocationDuration  *prometheus.HistogramVec
	GatewayInvocationsInFlight *prometheus.GaugeVec
	GatewayRecoveries          *prometheus.CounterVec

	DispatchWorkerConnections  *prometheus.GaugeVec
	DispatchWorkerStreams      *prometheus.CounterVec
	DispatchWorkers            *prometheus.GaugeVec
	DispatchAssignments        *prometheus.CounterVec
	DispatchPendingWork        *prometheus.GaugeVec
	DispatchTurns              *prometheus.CounterVec
	DispatchWorkerLosses       *prometheus.CounterVec
	DispatchEvents             *prometheus.CounterVec
	DispatchReconciles         *prometheus.CounterVec
	DispatchReconcileDuration  *prometheus.HistogramVec
	DispatchWorkspaceFreeBytes *prometheus.GaugeVec
	DispatchWorkspaceCapacity  *prometheus.GaugeVec
	DispatchWorkspaceFreeRatio *prometheus.GaugeVec
	DispatchWorkspaceWarning   *prometheus.GaugeVec
	DispatchWorkspaceGated     *prometheus.GaugeVec
}

// New creates one isolated registry and registers process, Go, build, HTTP,
// gateway, and dispatch collectors with a constant bounded component label.
func New(component, version, commit string) *Metrics {
	reg := prometheus.NewRegistry()
	registerer := prometheus.WrapRegistererWith(prometheus.Labels{"component": component}, reg)
	registerer.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	build := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "control_plane_build_info",
		Help: "Immutable control-plane build information.",
	}, []string{"version", "commit"})
	registerer.MustRegister(build)
	build.WithLabelValues(nonempty(version, "dev"), nonempty(commit, "none")).Set(1)

	m := &Metrics{
		Registry: reg,
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "control_plane_http_requests_total", Help: "HTTP requests by bounded listener, method, normalized route, and status class.",
		}, []string{"listener", "method", "route", "status_class"}),
		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "control_plane_http_request_duration_seconds", Help: "HTTP request duration by bounded listener, method, and normalized route.",
			Buckets: prometheus.DefBuckets,
		}, []string{"listener", "method", "route"}),
		HTTPInFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "control_plane_http_requests_in_flight", Help: "HTTP requests currently executing by listener.",
		}, []string{"listener"}),
		HTTPResponseBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "control_plane_http_response_bytes_total", Help: "HTTP response bytes by bounded listener and normalized route.",
		}, []string{"listener", "route"}),

		GatewayRunnerConnections: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "control_plane_gateway_runner_connections", Help: "Currently connected trusted runner streams.",
		}, []string{}),
		GatewayRunnerStreams: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "control_plane_gateway_runner_streams_total", Help: "Trusted runner streams by bounded terminal result.",
		}, []string{"result"}),
		GatewayInvocations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "control_plane_gateway_invocations_total", Help: "Tool invocations by bounded effect and result classes.",
		}, []string{"effect_class", "result"}),
		GatewayInvocationDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "control_plane_gateway_invocation_duration_seconds", Help: "Tool invocation duration by bounded effect and result classes.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}, []string{"effect_class", "result"}),
		GatewayInvocationsInFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "control_plane_gateway_invocations_in_flight", Help: "Tool invocations currently executing.",
		}, []string{"effect_class"}),
		GatewayRecoveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "control_plane_gateway_recoveries_total", Help: "Orphan invocation recoveries by bounded durable outcome, plus clean and error sweeps.",
		}, []string{"result"}),

		DispatchWorkerConnections: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "control_plane_dispatch_worker_connections", Help: "Currently connected harness worker streams.",
		}, []string{}),
		DispatchWorkerStreams: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "control_plane_dispatch_worker_streams_total", Help: "Harness worker streams by bounded terminal result.",
		}, []string{"result"}),
		DispatchWorkers: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "control_plane_dispatch_workers", Help: "Connected workers by bounded availability state (connected; busy/idle as of the last reconcile pass).",
		}, []string{"state"}),
		DispatchAssignments: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "control_plane_dispatch_assignments_total", Help: "Dispatch assignment attempts by bounded result.",
		}, []string{"result"}),
		DispatchPendingWork: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "control_plane_dispatch_pending_work", Help: "Pending work observed by the dispatch reconciler.",
		}, []string{}),
		DispatchTurns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "control_plane_dispatch_turns_total", Help: "Terminal turns by bounded outcome and reason.",
		}, []string{"outcome", "reason"}),
		DispatchWorkerLosses: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "control_plane_dispatch_worker_losses_total", Help: "Worker-loss terminalizations by bounded reason.",
		}, []string{"reason"}),
		DispatchEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "control_plane_dispatch_events_total", Help: "Worker events by bounded kind and result.",
		}, []string{"kind", "result"}),
		DispatchReconciles: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "control_plane_dispatch_reconciles_total", Help: "Dispatch reconciliation cycles by bounded result.",
		}, []string{"result"}),
		DispatchReconcileDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "control_plane_dispatch_reconcile_duration_seconds", Help: "Dispatch reconciliation cycle duration.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		}, []string{"result"}),
		DispatchWorkspaceFreeBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "control_plane_dispatch_workspace_free_bytes", Help: "Latest durable available bytes on one AgentPool PVC.",
		}, []string{"pool"}),
		DispatchWorkspaceCapacity: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "control_plane_dispatch_workspace_capacity_bytes", Help: "Latest durable total bytes on one AgentPool PVC.",
		}, []string{"pool"}),
		DispatchWorkspaceFreeRatio: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "control_plane_dispatch_workspace_free_ratio", Help: "Latest durable available-byte ratio on one AgentPool PVC.",
		}, []string{"pool"}),
		DispatchWorkspaceWarning: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "control_plane_dispatch_workspace_capacity_warning", Help: "Whether one AgentPool PVC is below the 25 percent warning threshold.",
		}, []string{"pool"}),
		DispatchWorkspaceGated: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "control_plane_dispatch_workspace_credit_gated", Help: "Whether one pool's durable 20/25 percent hysteresis gate withholds fresh credit.",
		}, []string{"pool"}),
	}
	registerer.MustRegister(
		m.HTTPRequests, m.HTTPDuration, m.HTTPInFlight, m.HTTPResponseBytes,
		m.GatewayRunnerConnections, m.GatewayRunnerStreams, m.GatewayInvocations,
		m.GatewayInvocationDuration, m.GatewayInvocationsInFlight, m.GatewayRecoveries,
		m.DispatchWorkerConnections, m.DispatchWorkerStreams, m.DispatchWorkers,
		m.DispatchAssignments, m.DispatchPendingWork,
		m.DispatchTurns, m.DispatchWorkerLosses, m.DispatchEvents,
		m.DispatchReconciles, m.DispatchReconcileDuration,
		m.DispatchWorkspaceFreeBytes, m.DispatchWorkspaceCapacity,
		m.DispatchWorkspaceFreeRatio, m.DispatchWorkspaceWarning, m.DispatchWorkspaceGated,
	)
	m.GatewayRunnerConnections.WithLabelValues().Set(0)
	m.DispatchWorkerConnections.WithLabelValues().Set(0)
	m.DispatchPendingWork.WithLabelValues().Set(0)
	return m
}

// Handler returns the metrics-only HTTP handler for an isolated registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

// HTTPMiddleware records bounded golden signals without changing optional
// ResponseWriter interfaces (streaming and artifact transfers remain intact).
func (m *Metrics) HTTPMiddleware(listener string) func(http.Handler) http.Handler {
	listener = bounded(listener)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m.HTTPInFlight.WithLabelValues(listener).Inc()
			defer m.HTTPInFlight.WithLabelValues(listener).Dec()
			started := time.Now()
			observed := httpsnoop.CaptureMetrics(next, w, r)
			route := chi.RouteContext(r.Context()).RoutePattern()
			if route == "" {
				route = "unmatched"
			}
			route = bounded(route)
			method := bounded(strings.ToUpper(r.Method))
			statusClass := strconv.Itoa(observed.Code/100) + "xx"
			m.HTTPRequests.WithLabelValues(listener, method, route, statusClass).Inc()
			m.HTTPDuration.WithLabelValues(listener, method, route).Observe(time.Since(started).Seconds())
			m.HTTPResponseBytes.WithLabelValues(listener, route).Add(float64(observed.Written))
		})
	}
}

// ProcedureMiddleware records Connect/gRPC requests using an explicit set of
// generated procedure constants. ServeMux handlers are mounted on service
// prefixes, so arbitrary suffixes and HTTP verbs must collapse to fixed labels.
func (m *Metrics) ProcedureMiddleware(listener string, procedures ...string) func(http.Handler) http.Handler {
	listener = bounded(listener)
	allowedProcedures := make(map[string]struct{}, len(procedures))
	for _, procedure := range procedures {
		allowedProcedures[procedure] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m.HTTPInFlight.WithLabelValues(listener).Inc()
			defer m.HTTPInFlight.WithLabelValues(listener).Dec()
			started := time.Now()
			observed := httpsnoop.CaptureMetrics(next, w, r)
			route := "unmatched"
			if _, ok := allowedProcedures[r.URL.Path]; ok {
				route = r.URL.Path
			}
			method := "other"
			if r.Method == http.MethodPost {
				method = http.MethodPost
			}
			statusClass := strconv.Itoa(observed.Code/100) + "xx"
			m.HTTPRequests.WithLabelValues(listener, method, route, statusClass).Inc()
			m.HTTPDuration.WithLabelValues(listener, method, route).Observe(time.Since(started).Seconds())
			m.HTTPResponseBytes.WithLabelValues(listener, route).Add(float64(observed.Written))
		})
	}
}

// RegisterDatabasePool exports pgx's in-memory pool statistics. Collection
// never performs a datastore operation and therefore cannot make scraping load
// or gate Postgres.
func (m *Metrics) RegisterDatabasePool(pool *pgxpool.Pool) {
	if pool == nil {
		return
	}
	m.Registry.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "control_plane_database_pool_max_connections", Help: "Configured maximum pgx pool connections."}, func() float64 { return float64(pool.Stat().MaxConns()) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "control_plane_database_pool_acquire_duration_seconds_total", Help: "Cumulative time waiting to acquire pgx pool connections."}, func() float64 { return pool.Stat().AcquireDuration().Seconds() }),
		&databasePoolCollector{pool: pool},
	)
}

type databasePoolCollector struct {
	pool *pgxpool.Pool
}

var databasePoolConnections = prometheus.NewDesc(
	"control_plane_database_pool_connections", "In-memory pgx pool connection counts by state.", []string{"state"}, nil,
)

func (c *databasePoolCollector) Describe(ch chan<- *prometheus.Desc) { ch <- databasePoolConnections }
func (c *databasePoolCollector) Collect(ch chan<- prometheus.Metric) {
	stat := c.pool.Stat()
	for state, value := range map[string]int32{
		"acquired": stat.AcquiredConns(), "idle": stat.IdleConns(), "total": stat.TotalConns(),
	} {
		ch <- prometheus.MustNewConstMetric(databasePoolConnections, prometheus.GaugeValue, float64(value), state)
	}
}

func nonempty(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func bounded(value string) string {
	if value == "" {
		return "unknown"
	}
	if len(value) > 128 {
		return value[:128]
	}
	return value
}

// StatusClass converts an HTTP status into a bounded Prometheus label.
func StatusClass(status int) string {
	if status < 100 || status > 599 {
		return "unknown"
	}
	return fmt.Sprintf("%dxx", status/100)
}
