package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrPolicyNotFound   = errors.New("operation policy not found")
	ErrDecisionConflict = errors.New("decision ID already used for another operation")
)

type Decision struct {
	Allowed       bool   `json:"allowed"`
	Reason        string `json:"reason"`
	UTCDay        string `json:"utc_day"`
	DailyUsed     int64  `json:"daily_used"`
	DailyLimit    int64  `json:"daily_limit"`
	PolicyVersion int64  `json:"policy_version"`
}

type Usage struct {
	Operation     string `json:"operation"`
	UTCDay        string `json:"utc_day"`
	DailyUsed     int64  `json:"daily_used"`
	DailyLimit    int64  `json:"daily_limit"`
	PolicyVersion int64  `json:"policy_version"`
	ResetsAtUTC   string `json:"resets_at_utc"`
}

type dbClock func(context.Context, pgx.Tx) (time.Time, error)

func postgresClock(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var now time.Time
	err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now)
	return now, err
}

func (s *Store) clockNow(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	if s.clock != nil {
		return s.clock(ctx, tx)
	}
	return postgresClock(ctx, tx)
}

func utcDay(now time.Time) string { return now.UTC().Format("2006-01-02") }

func nextUTC(now time.Time) string {
	year, month, day := now.UTC().Date()
	return time.Date(year, month, day+1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
}

// Decide returns a stored result for a repeated ID. The customer row lock
// serializes that lookup with new decisions for this customer.
func (s *Store) Decide(ctx context.Context, customerID, decisionID, operation string) (Decision, error) {
	return s.decide(ctx, customerID, decisionID, operation, nil)
}

// RateCheck runs only for a new decision, with the locked current policy.
// The caller must bound its Redis call by ctx. A false result is a rate denial.
type RateCheck func(context.Context, Policy) (bool, error)

func (s *Store) DecideWithRate(ctx context.Context, customerID, decisionID, operation string, check RateCheck) (Decision, error) {
	if check == nil {
		return Decision{}, errors.New("rate check is required")
	}
	return s.decide(ctx, customerID, decisionID, operation, check)
}

func (s *Store) decide(ctx context.Context, customerID, decisionID, operation string, check RateCheck) (Decision, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Decision{}, fmt.Errorf("begin decision: %w", err)
	}
	defer rollback(tx)
	if err := lockActiveCustomer(ctx, tx, customerID); err != nil {
		return Decision{}, err
	}

	var saved Decision
	var savedOperation string
	err = tx.QueryRow(ctx, `
		SELECT operation, allowed, reason, utc_day::text, daily_used, daily_limit, policy_version
		FROM quotagate.decisions WHERE customer_id = $1 AND decision_id = $2`,
		customerID, decisionID,
	).Scan(&savedOperation, &saved.Allowed, &saved.Reason, &saved.UTCDay, &saved.DailyUsed, &saved.DailyLimit, &saved.PolicyVersion)
	if err == nil {
		if savedOperation != operation {
			return Decision{}, ErrDecisionConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return Decision{}, fmt.Errorf("commit replay: %w", err)
		}
		return saved, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Decision{}, fmt.Errorf("read decision: %w", err)
	}

	policy := Policy{CustomerID: customerID, Operation: operation}
	err = tx.QueryRow(ctx, `SELECT daily_limit, rate_limit_per_minute, version FROM quotagate.policies WHERE customer_id = $1 AND operation = $2 FOR SHARE`, customerID, operation).Scan(&policy.DailyLimit, &policy.RateLimitPerMinute, &policy.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return Decision{}, ErrPolicyNotFound
	}
	if err != nil {
		return Decision{}, fmt.Errorf("read policy: %w", err)
	}
	// Keep the customer and policy locks across the bounded Redis call. This
	// serializes duplicates and policy changes, but does not roll back Redis.
	rateAllowed := true
	if check != nil {
		rateAllowed, err = check(ctx, policy)
		if err != nil {
			return Decision{}, fmt.Errorf("rate pre-check: %w", err)
		}
	}
	now, err := s.clockNow(ctx, tx)
	if err != nil {
		return Decision{}, fmt.Errorf("read database time: %w", err)
	}
	day := utcDay(now)
	decision := Decision{UTCDay: day, DailyLimit: policy.DailyLimit, PolicyVersion: policy.Version}
	if !rateAllowed {
		decision.Reason = "rate_limited"
		err = tx.QueryRow(ctx, `SELECT used FROM quotagate.daily_usage WHERE customer_id = $1 AND operation = $2 AND utc_day = $3::date`, customerID, operation, day).Scan(&decision.DailyUsed)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Decision{}, fmt.Errorf("read rate-denied usage: %w", err)
		}
	} else {
		err = tx.QueryRow(ctx, `
		INSERT INTO quotagate.daily_usage AS u (customer_id, operation, utc_day, used)
		VALUES ($1, $2, $3::date, 1)
		ON CONFLICT (customer_id, operation, utc_day) DO UPDATE SET used = u.used + 1
		WHERE u.used < $4
		RETURNING used`, customerID, operation, day, policy.DailyLimit,
		).Scan(&decision.DailyUsed)
		if errors.Is(err, pgx.ErrNoRows) {
			decision.Allowed = false
			decision.Reason = "daily_quota"
			err = tx.QueryRow(ctx, `SELECT used FROM quotagate.daily_usage WHERE customer_id = $1 AND operation = $2 AND utc_day = $3::date`, customerID, operation, day).Scan(&decision.DailyUsed)
			if err != nil {
				return Decision{}, fmt.Errorf("read exhausted usage: %w", err)
			}
		} else if err != nil {
			return Decision{}, fmt.Errorf("reserve daily usage: %w", err)
		} else {
			decision.Allowed = true
			decision.Reason = "allowed"
		}
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO quotagate.decisions
		(customer_id, decision_id, operation, utc_day, allowed, reason, daily_used, daily_limit, policy_version)
		VALUES ($1, $2, $3, $4::date, $5, $6, $7, $8, $9)`,
		customerID, decisionID, operation, day, decision.Allowed, decision.Reason,
		decision.DailyUsed, decision.DailyLimit, decision.PolicyVersion,
	)
	if err != nil {
		return Decision{}, fmt.Errorf("record decision: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Decision{}, fmt.Errorf("commit decision: %w", err)
	}
	return decision, nil
}

func (s *Store) Usage(ctx context.Context, customerID, operation string) (Usage, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Usage{}, fmt.Errorf("begin usage read: %w", err)
	}
	defer rollback(tx)
	if err := lockActiveCustomer(ctx, tx, customerID); err != nil {
		return Usage{}, err
	}
	usage := Usage{Operation: operation}
	err = tx.QueryRow(ctx, `SELECT daily_limit, version FROM quotagate.policies WHERE customer_id = $1 AND operation = $2 FOR SHARE`, customerID, operation).Scan(&usage.DailyLimit, &usage.PolicyVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return Usage{}, ErrPolicyNotFound
	}
	if err != nil {
		return Usage{}, fmt.Errorf("read usage policy: %w", err)
	}
	now, err := s.clockNow(ctx, tx)
	if err != nil {
		return Usage{}, fmt.Errorf("read database time: %w", err)
	}
	usage.UTCDay, usage.ResetsAtUTC = utcDay(now), nextUTC(now)
	err = tx.QueryRow(ctx, `SELECT used FROM quotagate.daily_usage WHERE customer_id = $1 AND operation = $2 AND utc_day = $3::date`, customerID, operation, usage.UTCDay).Scan(&usage.DailyUsed)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Usage{}, fmt.Errorf("read daily usage: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Usage{}, fmt.Errorf("commit usage read: %w", err)
	}
	return usage, nil
}

func lockActiveCustomer(ctx context.Context, tx pgx.Tx, customerID string) error {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM quotagate.customers WHERE id = $1 AND disabled_at IS NULL FOR UPDATE`, customerID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock customer: %w", err)
	}
	return nil
}

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
