package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"quotagate/internal/ratelimit"
	"quotagate/internal/store"
)

const observationKeyToken = "qg_" + "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"

type observationStore struct {
	CustomerStore
	authErr   error
	decideErr error
	decision  store.Decision
	delay     time.Duration
}

func (s observationStore) Authenticate(context.Context, string) (store.Customer, error) {
	return store.Customer{ID: "customer-private"}, s.authErr
}
func (s observationStore) CreateCustomer(context.Context, string) (store.Customer, string, error) {
	return store.Customer{ID: "customer-private"}, observationKeyToken, nil
}
func (s observationStore) GetPolicy(context.Context, string, string) (store.Policy, error) {
	return store.Policy{CustomerID: "customer-private", Operation: "job.create"}, nil
}
func (s observationStore) Decide(context.Context, string, string, string) (store.Decision, error) {
	time.Sleep(s.delay)
	return s.decision, s.decideErr
}
func (s observationStore) DecideWithRate(ctx context.Context, customer, id, operation string, check store.RateCheck) (store.Decision, error) {
	if s.decideErr != nil {
		return store.Decision{}, s.decideErr
	}
	_, err := check(ctx, store.Policy{CustomerID: customer, Operation: operation, RateLimitPerMinute: 3})
	return s.decision, err
}

type observationRate struct{ pingErr, checkErr error }

func (r observationRate) Ping(context.Context) error { return r.pingErr }
func (r observationRate) Check(context.Context, string, string, string, int64) (ratelimit.Result, error) {
	return ratelimit.Result{Allowed: true}, r.checkErr
}

func observationLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buffer bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buffer, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buffer
}

func observationRequest(h http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	// Untrusted correlation IDs must not make it into the response or log.
	r.Header.Set("X-Request-ID", observationKeyToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func observationMetrics(t *testing.T, h http.Handler) (map[string]*dto.MetricFamily, string) {
	t.Helper()
	w := observationRequest(h, "GET", "/metrics", "", "")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("metrics status/type = %d/%s", w.Code, w.Header().Get("Content-Type"))
	}
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(w.Body.String()))
	if err != nil {
		t.Fatalf("invalid Prometheus exposition: %v", err)
	}
	return families, w.Body.String()
}

func observationMetric(families map[string]*dto.MetricFamily, name string, labels map[string]string) *dto.Metric {
	for _, metric := range families[name].GetMetric() {
		matches := len(metric.Label) == len(labels)
		for _, label := range metric.Label {
			value, exists := labels[label.GetName()]
			matches = matches && exists && value == label.GetValue()
		}
		if matches {
			return metric
		}
	}
	return nil
}

func TestObservedDecisionOutcomesAndDependencyAttribution(t *testing.T) {
	for _, tc := range []struct {
		name, reason, outcome, dependency, operation string
		authErr, decideErr, redisErr                 error
		status                                       int
	}{
		{name: "allow", reason: "allowed", outcome: "allowed", status: 200},
		{name: "daily", reason: "daily_quota", outcome: "daily_quota", status: 200},
		{name: "rate", reason: "rate_limited", outcome: "rate_limited", status: 200},
		{name: "redis outage", redisErr: errors.New(observationKeyToken), outcome: "unavailable", dependency: "redis", operation: "check", status: 503},
		{name: "postgres auth outage", authErr: errors.New(observationKeyToken), outcome: "unavailable", dependency: "postgres", operation: "authenticate", status: 503},
		{name: "postgres transaction outage", decideErr: errors.New(observationKeyToken), outcome: "unavailable", dependency: "postgres", operation: "decide", status: 503},
		{name: "revoked key", authErr: store.ErrNotFound, outcome: "unauthorized", status: 401},
		{name: "missing policy", decideErr: store.ErrPolicyNotFound, outcome: "forbidden", status: 403},
		{name: "decision conflict", decideErr: store.ErrDecisionConflict, outcome: "conflict", status: 409},
		{name: "redis pre-result conflict", redisErr: ratelimit.ErrDecisionConflict, outcome: "conflict", status: 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := observationLogs(t)
			s := observationStore{authErr: tc.authErr, decideErr: tc.decideErr, decision: store.Decision{Allowed: tc.reason == "allowed", Reason: tc.reason}}
			h := NewCombinedAppHandler(fakePinger{}, s, "admin-private", observationRate{checkErr: tc.redisErr})
			w := observationRequest(h, "POST", "/v1/decisions", observationKeyToken, `{"decision_id":"private_decision_000001","operation":"job.create"}`)
			if w.Code != tc.status {
				t.Fatalf("decision status = %d, want %d", w.Code, tc.status)
			}
			families, text := observationMetrics(t, h)
			if got := observationMetric(families, "qg_decision_responses_total", map[string]string{"outcome": tc.outcome}).GetCounter().GetValue(); got != 1 {
				t.Fatalf("outcome count = %v, want 1", got)
			}
			var dependencyTotal float64
			for _, metric := range families["qg_dependency_errors_total"].GetMetric() {
				dependencyTotal += metric.GetCounter().GetValue()
			}
			wantFailures := float64(0)
			if tc.dependency != "" {
				wantFailures = 1
				if observationMetric(families, "qg_dependency_errors_total", map[string]string{"dependency": tc.dependency, "operation": tc.operation}).GetCounter().GetValue() != 1 {
					t.Fatal("dependency error was attributed to the wrong operation")
				}
			}
			if dependencyTotal != wantFailures {
				t.Fatalf("dependency total = %v, want %v", dependencyTotal, wantFailures)
			}
			var event map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &event); err != nil {
				t.Fatalf("invalid JSON log: %v", err)
			}
			if event["correlation_id"] != w.Header().Get("X-Request-ID") || event["decision_outcome"] != tc.outcome || event["status"] != float64(tc.status) {
				t.Fatal("response and log correlation/outcome/status differ")
			}
			for _, secret := range []string{observationKeyToken, "customer-private", "private_decision_000001", "admin-private"} {
				if strings.Contains(text+logs.String(), secret) {
					t.Fatal("secret or customer/decision ID leaked into metrics or logs")
				}
			}
		})
	}
}

func TestObservedRoutesHistogramReplayAndRegistryIsolation(t *testing.T) {
	logs := observationLogs(t)
	s := observationStore{decision: store.Decision{Allowed: true, Reason: "allowed"}, delay: 15 * time.Millisecond}
	h := NewAppHandler(fakePinger{}, s, "admin-private")
	ids := map[string]bool{}
	for range 2 {
		w := observationRequest(h, "POST", "/v1/decisions", observationKeyToken, `{"decision_id":"private_decision_000001","operation":"job.create"}`)
		id := w.Header().Get("X-Request-ID")
		if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(id) || ids[id] {
			t.Fatal("correlation ID missing or reused for separate HTTP requests")
		}
		ids[id] = true
	}
	for i := range 30 {
		observationRequest(h, "GET", fmt.Sprintf("/v1/policies/job.private%d", i), observationKeyToken, "")
		observationRequest(h, "PRIVATE", fmt.Sprintf("/missing/customer-private%d?token=%s", i, observationKeyToken), "", "")
	}
	observationRequest(h, "POST", "/v1/admin/customers", "admin-private", `{"name":"customer-private"}`)
	observationRequest(h, "POST", "/v1/decisions", observationKeyToken, "malformed-"+observationKeyToken)
	before, _ := observationMetrics(t, h)
	after, text := observationMetrics(t, h)
	if observationMetric(after, "qg_decision_responses_total", map[string]string{"outcome": "allowed"}).GetCounter().GetValue() != 2 {
		t.Fatal("replayed HTTP decision was not counted separately")
	}
	histogram := observationMetric(after, "qg_http_request_duration_seconds", map[string]string{"route": "/v1/decisions", "method": "POST"}).GetHistogram()
	if histogram.GetSampleCount() != 3 || histogram.GetSampleSum() < 0.03 {
		t.Fatal("request histogram missed decision handler time or error response")
	}
	for _, bucket := range histogram.GetBucket() {
		if bucket.GetUpperBound() == 0.01 && bucket.GetCumulativeCount() > 1 {
			t.Fatal("slow handlers were assigned to a bucket shorter than their duration")
		}
	}
	if len(before["qg_http_requests_total"].Metric) != len(after["qg_http_requests_total"].Metric) || len(after["qg_http_requests_total"].Metric) != 5 {
		t.Fatal("caller paths or metrics scrapes increased metric cardinality")
	}
	for _, secret := range []string{"customer-private", "admin-private", observationKeyToken, "job.private", "private_decision_000001"} {
		if strings.Contains(text+logs.String(), secret) {
			t.Fatal("untrusted path, header, body or response appeared in telemetry")
		}
	}
	independent := NewAppHandler(fakePinger{}, s, "admin-private")
	zero, _ := observationMetrics(t, independent)
	if observationMetric(zero, "qg_decision_responses_total", map[string]string{"outcome": "allowed"}).GetCounter().GetValue() != 0 {
		t.Fatal("handler instances shared a registry")
	}
}

func TestObservedReadinessReportsEachDependency(t *testing.T) {
	observationLogs(t)
	for _, tc := range []struct {
		name       string
		postgres   error
		redis      error
		pg, rd, ok float64
		status     int
	}{
		{"healthy", nil, nil, 1, 1, 1, 200},
		{"postgres down", errors.New("private DSN"), nil, 0, 1, 0, 503},
		{"redis down", nil, errors.New("private redis URL"), 1, 0, 0, 503},
		{"both down", errors.New("private DSN"), errors.New("private redis URL"), 0, 0, 0, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewCombinedAppHandler(fakePinger{err: tc.postgres}, observationStore{}, "admin-private", observationRate{pingErr: tc.redis})
			unknown, _ := observationMetrics(t, h)
			if observationMetric(unknown, "qg_ready", nil).GetGauge().GetValue() != -1 {
				t.Fatal("unprobed readiness was presented as healthy")
			}
			if w := observationRequest(h, "GET", "/health/ready", "", ""); w.Code != tc.status {
				t.Fatalf("readiness status = %d, want %d", w.Code, tc.status)
			}
			if w := observationRequest(h, "GET", "/health/live", "", ""); w.Code != 200 {
				t.Fatal("dependency failure changed liveness")
			}
			metrics, _ := observationMetrics(t, h)
			for dependency, want := range map[string]float64{"postgres": tc.pg, "redis": tc.rd} {
				if observationMetric(metrics, "qg_dependency_ready", map[string]string{"dependency": dependency}).GetGauge().GetValue() != want {
					t.Fatalf("%s readiness gauge mismatch", dependency)
				}
			}
			if observationMetric(metrics, "qg_ready", nil).GetGauge().GetValue() != tc.ok {
				t.Fatal("overall readiness gauge mismatch")
			}
		})
	}
}

func TestObservedConcurrentRequestsAndScrapes(t *testing.T) {
	observationLogs(t)
	h := NewAppHandler(fakePinger{}, observationStore{decision: store.Decision{Allowed: true, Reason: "allowed"}}, "admin-private")
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Go(func() {
			w := observationRequest(h, "POST", "/v1/decisions", observationKeyToken, fmt.Sprintf(`{"decision_id":"concurrent_private_%04d","operation":"job.create"}`, i))
			if w.Code != 200 {
				t.Errorf("concurrent status = %d", w.Code)
			}
		})
		if i%10 == 0 {
			wg.Go(func() { observationRequest(h, "GET", "/metrics", "", "") })
		}
	}
	wg.Wait()
	families, _ := observationMetrics(t, h)
	if observationMetric(families, "qg_decision_responses_total", map[string]string{"outcome": "allowed"}).GetCounter().GetValue() != 100 {
		t.Fatal("concurrent observations lost decision responses")
	}
	if observationMetric(families, "qg_http_request_duration_seconds", map[string]string{"route": "/v1/decisions", "method": "POST"}).GetHistogram().GetSampleCount() != 100 {
		t.Fatal("concurrent observations lost histogram samples")
	}
}
