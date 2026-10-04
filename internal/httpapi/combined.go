package httpapi

import (
	"context"
	"errors"
	"net/http"

	"quotagate/internal/ratelimit"
	"quotagate/internal/store"
)

type CombinedStore interface {
	CustomerStore
	DecideWithRate(context.Context, string, string, string, store.RateCheck) (store.Decision, error)
}

type combinedStore struct {
	CombinedStore
	rate RateLimiter
}

func (s combinedStore) Decide(ctx context.Context, customerID, decisionID, operation string) (store.Decision, error) {
	return s.DecideWithRate(ctx, customerID, decisionID, operation, func(ctx context.Context, policy store.Policy) (bool, error) {
		result, err := s.rate.Check(ctx, customerID, decisionID, operation, int64(policy.RateLimitPerMinute))
		if errors.Is(err, ratelimit.ErrDecisionConflict) {
			return false, store.ErrDecisionConflict
		}
		return result.Allowed, err
	})
}

// NewCombinedAppHandler enables durable rate + daily decisions. The separate
// preliminary demo endpoint is exposed only by NewRateAppHandler.
func NewCombinedAppHandler(db Pinger, customers CombinedStore, adminToken string, rate RateLimiter) http.Handler {
	return newAppHandler(rateDependencies{db, rate}, combinedStore{customers, rate}, adminToken, nil)
}
