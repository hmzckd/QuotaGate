package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"quotagate/internal/ratelimit"
	"quotagate/internal/store"
)

type observation struct {
	registry   *prometheus.Registry
	requests   *prometheus.CounterVec
	duration   *prometheus.HistogramVec
	decisions  *prometheus.CounterVec
	errors     *prometheus.CounterVec
	dependency *prometheus.GaugeVec
	ready      prometheus.Gauge
	logger     *slog.Logger
}

func newObservation() *observation {
	o := &observation{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "qg_http_requests_total", Help: "Completed HTTP handlers by selected response status, excluding metrics scrapes.",
		}, []string{"route", "method", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "qg_http_request_duration_seconds", Help: "HTTP handler duration, including dependency waits, excluding metrics scrapes.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5},
		}, []string{"route", "method"}),
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "qg_decision_responses_total", Help: "Decision response attempts by outcome; retries and replays count separately.",
		}, []string{"outcome"}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "qg_dependency_errors_total", Help: "Failed dependency operations, excluding business rejections.",
		}, []string{"dependency", "operation"}),
		dependency: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "qg_dependency_ready", Help: "Last readiness probe: 1 ready, 0 failed, -1 not yet probed.",
		}, []string{"dependency"}),
		ready: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "qg_ready", Help: "Last overall readiness probe: 1 ready, 0 failed, -1 not yet probed.",
		}),
		logger: slog.Default(),
	}
	o.registry.MustRegister(o.requests, o.duration, o.decisions, o.errors, o.dependency, o.ready)
	o.ready.Set(-1)
	for _, outcome := range []string{"allowed", "rate_limited", "daily_quota", "unavailable", "unauthorized", "forbidden", "conflict", "bad_request", "other"} {
		o.decisions.WithLabelValues(outcome)
	}
	return o
}

type requestObservation struct {
	outcome       string
	postgresError bool
	redisError    bool
}

type observationKey struct{}

func decisionOutcome(r *http.Request, reason string) {
	if state, ok := r.Context().Value(observationKey{}).(*requestObservation); ok {
		switch reason {
		case "allowed", "daily_quota", "rate_limited":
			state.outcome = reason
		}
	}
}

type dependencyError struct{ cause error }

func (e *dependencyError) Error() string { return "Redis decision check unavailable" }
func (e *dependencyError) Unwrap() error { return e.cause }

func (o *observation) failure(ctx context.Context, dependency, operation string, err error) {
	if err == nil || errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrPolicyNotFound) ||
		errors.Is(err, store.ErrDecisionConflict) || errors.Is(err, ratelimit.ErrDecisionConflict) {
		return
	}
	var redisFailure *dependencyError
	if errors.As(err, &redisFailure) {
		dependency, operation = "redis", "check"
	}
	o.errors.WithLabelValues(dependency, operation).Inc()
	if state, ok := ctx.Value(observationKey{}).(*requestObservation); ok {
		if dependency == "postgres" {
			state.postgresError = true
		} else {
			state.redisError = true
		}
	}
}

// Both probes share the health handler's one-second deadline and run concurrently.
// Readiness gauges describe only these probes, not a durable availability claim.
type observedReadiness struct {
	db   Pinger
	rate Pinger
	obs  *observation
}

func (d observedReadiness) Ping(ctx context.Context) error {
	var redisResult chan error
	if d.rate != nil {
		redisResult = make(chan error, 1)
		go func() { redisResult <- d.rate.Ping(ctx) }()
	}
	dbErr := d.db.Ping(ctx)
	d.obs.probe(ctx, "postgres", dbErr)
	var redisErr error
	if redisResult != nil {
		redisErr = <-redisResult
		d.obs.probe(ctx, "redis", redisErr)
	}
	err := errors.Join(dbErr, redisErr)
	if err == nil {
		d.obs.ready.Set(1)
	} else {
		d.obs.ready.Set(0)
	}
	return err
}

func (o *observation) probe(ctx context.Context, dependency string, err error) {
	if err == nil {
		o.dependency.WithLabelValues(dependency).Set(1)
	} else {
		o.dependency.WithLabelValues(dependency).Set(0)
		o.failure(ctx, dependency, "ping", err)
	}
}

// Unwrap preserves access through http.ResponseController. These endpoints do
// not require streaming or hijacking; writes still go directly to the caller.
type observedResponse struct {
	http.ResponseWriter
	status int
}

func (w *observedResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *observedResponse) WriteHeader(status int) {
	if status >= 100 && status < 200 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *observedResponse) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (o *observation) wrap(mux *http.ServeMux) http.Handler {
	mux.Handle("GET /metrics", promhttp.HandlerFor(o.registry, promhttp.HandlerOpts{}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		var id [16]byte
		_, _ = rand.Read(id[:]) // crypto/rand.Read fails irrecoverably on modern Go.
		correlation := hex.EncodeToString(id[:])
		w.Header().Set("X-Request-ID", correlation)
		state := &requestObservation{}
		r = r.WithContext(context.WithValue(r.Context(), observationKey{}, state))
		response := &observedResponse{ResponseWriter: w}
		mux.ServeHTTP(response, r)
		route := observedRoute(r.Pattern)
		if route == "/metrics" {
			return
		}
		status := response.status
		if status == 0 {
			status = http.StatusOK
		}
		method := observedMethod(r.Method)
		duration := time.Since(started).Seconds()
		o.requests.WithLabelValues(route, method, strconv.Itoa(status)).Inc()
		o.duration.WithLabelValues(route, method).Observe(duration)
		attrs := []any{"correlation_id", correlation, "route", route, "method", method, "status", status, "duration_seconds", duration,
			"postgres_error", state.postgresError, "redis_error", state.redisError}
		if route == "/v1/decisions" {
			outcome := state.outcome
			if outcome == "" {
				outcome = errorOutcome(status)
			}
			o.decisions.WithLabelValues(outcome).Inc()
			attrs = append(attrs, "decision_outcome", outcome)
		}
		o.logger.InfoContext(r.Context(), "http_request", attrs...)
	})
}

// Never use URL paths, query strings, operation names or caller-supplied IDs.
// This is an allowlist: when a new route is registered on the mux, add its
// pattern here too, or its metrics and logs silently report "unmatched".
func observedRoute(pattern string) string {
	_, route, _ := strings.Cut(pattern, " ")
	switch route {
	case "/health/live", "/health/ready", "/metrics", "/v1/admin/customers",
		"/v1/admin/customers/{customerID}/rotate-key", "/v1/admin/customers/{customerID}/disable",
		"/v1/admin/customers/{customerID}/policies/{operation}", "/v1/me", "/v1/policies/{operation}",
		"/v1/decisions", "/v1/usage", "/demo/rate-checks":
		return route
	default:
		return "unmatched"
	}
}

func observedMethod(method string) string {
	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
		return method
	default:
		return "OTHER"
	}
}

func errorOutcome(status int) string {
	switch status {
	case http.StatusServiceUnavailable:
		return "unavailable"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusConflict:
		return "conflict"
	case http.StatusBadRequest:
		return "bad_request"
	default:
		return "other"
	}
}
