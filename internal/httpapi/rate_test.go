package httpapi

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"quotagate/internal/ratelimit"
	"quotagate/internal/store"
)

type rateCustomerStore struct {
	CustomerStore
	policyErr error
}

func (s rateCustomerStore) Authenticate(context.Context, string) (store.Customer, error) {
	return store.Customer{ID: "customer-from-key"}, nil
}

func (s rateCustomerStore) GetPolicy(ctx context.Context, customer, operation string) (store.Policy, error) {
	return store.Policy{CustomerID: customer, Operation: operation, RateLimitPerMinute: 3}, s.policyErr
}

type fakeRateLimiter struct {
	result ratelimit.Result
	err    error
	called int
	t      *testing.T
}

func (f *fakeRateLimiter) Ping(context.Context) error { return f.err }

func (f *fakeRateLimiter) Check(ctx context.Context, customer, id, operation string, limit int64) (ratelimit.Result, error) {
	f.called++
	if customer != "customer-from-key" || id != "demo_decision_000001" || operation != "job.create" || limit != 3 {
		f.t.Error("rate check did not use the authenticated customer's policy")
	}
	return f.result, f.err
}

func TestRateDemoScopeAndErrors(t *testing.T) {
	const body = `{"decision_id":"demo_decision_000001","operation":"job.create"}`
	for _, tc := range []struct {
		name      string
		body      string
		query     string
		policyErr error
		rateErr   error
		want      int
		calls     int
	}{
		{"pre-result", body, "", nil, nil, 200, 1},
		{"foreign body customer", `{"decision_id":"demo_decision_000001","operation":"job.create","customer_id":"foreign"}`, "", nil, nil, 400, 0},
		{"foreign query customer", body, "?customer_id=foreign", nil, nil, 400, 0},
		{"bad decision ID", `{"decision_id":"short","operation":"job.create"}`, "", nil, nil, 400, 0},
		{"missing policy", body, "", store.ErrNotFound, nil, 403, 0},
		{"policy outage", body, "", errors.New("private DB error"), nil, 503, 0},
		{"Redis outage", body, "", nil, errors.New("private Redis error"), 503, 1},
		{"operation conflict", body, "", nil, ratelimit.ErrDecisionConflict, 409, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limiter := &fakeRateLimiter{t: t, err: tc.rateErr, result: ratelimit.Result{Allowed: false, Reason: "rate_limited", RateUsed: 3, RateLimit: 3}}
			handler := NewRateAppHandler(fakePinger{}, rateCustomerStore{policyErr: tc.policyErr}, "unused-local-admin", limiter)
			r := httptest.NewRequest("POST", "/demo/rate-checks"+tc.query, strings.NewReader(tc.body))
			r.Header.Set("Authorization", "Bearer qg_"+strings.Repeat("a", 64))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.want || limiter.called != tc.calls {
				t.Fatalf("status=%d calls=%d, want %d/%d", w.Code, limiter.called, tc.want, tc.calls)
			}
			if strings.Contains(w.Body.String(), "private") {
				t.Error("dependency error details leaked to caller")
			}
		})
	}
}

func TestRateDemoReadinessAndModeBoundary(t *testing.T) {
	limiter := &fakeRateLimiter{t: t, err: errors.New("Redis down")}
	handler := NewRateAppHandler(fakePinger{}, rateCustomerStore{}, "unused-local-admin", limiter)
	for path, want := range map[string]int{"/health/live": 200, "/health/ready": 503} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != want {
			t.Fatalf("%s status = %d, want %d", path, w.Code, want)
		}
	}
	w := httptest.NewRecorder()
	NewAppHandler(fakePinger{}, rateCustomerStore{}, "unused-local-admin").ServeHTTP(w, httptest.NewRequest("POST", "/demo/rate-checks", nil))
	if w.Code != 404 {
		t.Fatal("v0.1 mode exposed the preliminary rate-check endpoint")
	}
}
