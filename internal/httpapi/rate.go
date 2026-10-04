package httpapi

import (
	"context"
	"errors"
	"net/http"

	"quotagate/internal/ratelimit"
	"quotagate/internal/store"
)

type RateLimiter interface {
	Check(context.Context, string, string, string, int64) (ratelimit.Result, error)
	Ping(context.Context) error
}

type rateDependencies struct {
	db   Pinger
	rate RateLimiter
}

func (d rateDependencies) Ping(ctx context.Context) error {
	if err := d.db.Ping(ctx); err != nil {
		return err
	}
	return d.rate.Ping(ctx)
}

// NewRateAppHandler enables the QG-05 pre-check demo and Redis readiness.
// /v1/decisions keeps its v0.1 daily-only contract until QG-06.
func NewRateAppHandler(db Pinger, customers CustomerStore, adminToken string, rate RateLimiter) http.Handler {
	return newAppHandler(rateDependencies{db, rate}, customers, adminToken, rate)
}

func (m *management) rateCheck(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "query parameters are not accepted")
		return
	}
	customer, ok := m.customer(w, r)
	if !ok {
		return
	}
	var body struct {
		DecisionID string `json:"decision_id"`
		Operation  string `json:"operation"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if !decisionName.MatchString(body.DecisionID) || !validOperation(body.Operation) {
		writeError(w, http.StatusBadRequest, "invalid decision ID or operation")
		return
	}
	ctx, cancel := requestContext(r)
	defer cancel()
	policy, err := m.store.GetPolicy(ctx, customer.ID, body.Operation)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusForbidden, "operation policy not found")
		} else {
			writeError(w, http.StatusServiceUnavailable, "policy storage unavailable")
		}
		return
	}
	result, err := m.rate.Check(ctx, customer.ID, body.DecisionID, body.Operation, int64(policy.RateLimitPerMinute))
	if err != nil {
		if errors.Is(err, ratelimit.ErrDecisionConflict) {
			writeError(w, http.StatusConflict, "decision ID already used for another operation")
		} else {
			writeError(w, http.StatusServiceUnavailable, "rate storage unavailable")
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}
