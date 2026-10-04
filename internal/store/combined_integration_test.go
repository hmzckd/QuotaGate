package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"quotagate/internal/ratelimit"
)

func combinedRedis(t *testing.T) (*ratelimit.Limiter, *redis.Client) {
	t.Helper()
	url := os.Getenv("QG_TEST_REDIS_URL")
	if url == "" {
		t.Skip("set QG_TEST_REDIS_URL for combined PostgreSQL + Redis tests")
	}
	limiter, err := ratelimit.New(url)
	if err != nil {
		t.Fatal(err)
	}
	options, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	raw := redis.NewClient(options)
	t.Cleanup(func() { _ = limiter.Close(); _ = raw.Close() })
	return limiter, raw
}

func combinedCustomer(t *testing.T, s *Store, pool *pgxpool.Pool, raw *redis.Client, daily int64, rate int32) Customer {
	t.Helper()
	customer := testCustomer(t, s, pool, daily)
	if _, err := s.PutPolicy(context.Background(), Policy{customer.ID, "job.create", daily, rate, 0}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var cursor uint64
		for {
			keys, next, err := raw.Scan(ctx, cursor, fmt.Sprintf("qg:rate:%x:*", sha256.Sum256([]byte(customer.ID))), 100).Result()
			if err != nil {
				t.Errorf("clean Redis test keys: %v", err)
				return
			}
			if len(keys) > 0 {
				if err := raw.Del(ctx, keys...).Err(); err != nil {
					t.Errorf("delete Redis test keys: %v", err)
				}
			}
			cursor = next
			if cursor == 0 {
				return
			}
		}
	})
	return customer
}

func rateCheck(limiter *ratelimit.Limiter, decisionID string) RateCheck {
	return func(ctx context.Context, policy Policy) (bool, error) {
		result, err := limiter.Check(ctx, policy.CustomerID, decisionID, policy.Operation, int64(policy.RateLimitPerMinute))
		if errors.Is(err, ratelimit.ErrDecisionConflict) {
			return false, ErrDecisionConflict
		}
		return result.Allowed, err
	}
}

func safeCombinedWindow(t *testing.T, raw *redis.Client) {
	t.Helper()
	now, err := raw.Time(context.Background()).Result()
	if err != nil {
		t.Fatal(err)
	}
	remaining := now.Truncate(time.Minute).Add(time.Minute).Sub(now)
	if remaining < 10*time.Second {
		time.Sleep(remaining + 20*time.Millisecond)
	}
}

func assertCombinedCounts(t *testing.T, pool *pgxpool.Pool, customerID string, used, decisions, allowed int64) {
	t.Helper()
	var gotUsed, gotDecisions, gotAllowed int64
	err := pool.QueryRow(context.Background(), `SELECT
		COALESCE((SELECT sum(used) FROM quotagate.daily_usage WHERE customer_id=$1),0),
		(SELECT count(*) FROM quotagate.decisions WHERE customer_id=$1),
		(SELECT count(*) FROM quotagate.decisions WHERE customer_id=$1 AND allowed)`, customerID).Scan(&gotUsed, &gotDecisions, &gotAllowed)
	if err != nil || gotUsed != used || gotDecisions != decisions || gotAllowed != allowed {
		t.Fatalf("daily/decisions/allowed=%d/%d/%d, want %d/%d/%d; err=%v", gotUsed, gotDecisions, gotAllowed, used, decisions, allowed, err)
	}
}

func assertRateUsed(t *testing.T, raw *redis.Client, customerID string, used int64) {
	t.Helper()
	key := fmt.Sprintf("qg:rate:%x:bucket:%x", sha256.Sum256([]byte(customerID)), sha256.Sum256([]byte("job.create")))
	got, err := raw.HGet(context.Background(), key, "used").Int64()
	if err != nil || got != used {
		t.Fatalf("rate used=%d, want %d; err=%v", got, used, err)
	}
}

func TestCombinedConcurrentLimitsAndDuplicates(t *testing.T) {
	s, pool := testStore(t)
	limiter, raw := combinedRedis(t)
	for _, tc := range []struct {
		name                                     string
		daily                                    int64
		requests, allow, dailyDenial, rateDenial int
		duplicate                                bool
	}{
		{"rate boundary", 1000, 60, 7, 0, 53, false},
		{"daily boundary", 3, 60, 3, 4, 53, false},
		{"duplicate ID", 1, 20, 20, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			customer := combinedCustomer(t, s, pool, raw, tc.daily, 7)
			safeCombinedWindow(t, raw)
			results := make(chan Decision, tc.requests)
			errCh := make(chan error, tc.requests)
			var wg sync.WaitGroup
			for i := range tc.requests {
				wg.Go(func() {
					id := fmt.Sprintf("combined_%024d", i)
					if tc.duplicate {
						id = "combined_duplicate_0001"
					}
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					// A separate Store instance represents the second API process.
					instance := s
					if i%2 == 1 {
						instance = New(pool)
					}
					decision, err := instance.DecideWithRate(ctx, customer.ID, id, "job.create", rateCheck(limiter, id))
					if err != nil {
						errCh <- err
					} else {
						results <- decision
					}
				})
			}
			wg.Wait()
			close(results)
			close(errCh)
			for err := range errCh {
				t.Errorf("combined decision: %v", err)
			}
			counts := map[string]int{}
			for decision := range results {
				counts[decision.Reason]++
			}
			if counts["allowed"] != tc.allow || counts["daily_quota"] != tc.dailyDenial || counts["rate_limited"] != tc.rateDenial {
				t.Fatalf("reason counts=%v", counts)
			}
			used, records, rateUsed := int64(tc.allow), int64(tc.requests), int64(7)
			if tc.duplicate {
				used, records, rateUsed = 1, 1, 1
			}
			assertCombinedCounts(t, pool, customer.ID, used, records, used)
			assertRateUsed(t, raw, customer.ID, rateUsed)
		})
	}
}

func TestCombinedPartialFailuresAndDurableReplay(t *testing.T) {
	s, pool := testStore(t)
	limiter, raw := combinedRedis(t)
	customer := combinedCustomer(t, s, pool, raw, 1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	safeCombinedWindow(t, raw)
	const id = "combined_fault_000001"

	// Redis finishes its Lua script but its reply is lost before PostgreSQL writes.
	lostReply := func(ctx context.Context, policy Policy) (bool, error) {
		if _, err := rateCheck(limiter, id)(ctx, policy); err != nil {
			return false, err
		}
		return false, errors.New("injected lost Redis reply")
	}
	if _, err := s.DecideWithRate(ctx, customer.ID, id, "job.create", lostReply); err == nil {
		t.Fatal("lost Redis reply should fail closed")
	}
	assertCombinedCounts(t, pool, customer.ID, 0, 0, 0)
	assertRateUsed(t, raw, customer.ID, 1)

	// Reject this customer's decision INSERT after the daily increment. The real
	// PostgreSQL constraint error must roll back both durable writes, never Redis.
	constraint := pgx.Identifier{"qg06_fault_" + customer.ID[4:]}.Sanitize()
	ddl := fmt.Sprintf("ALTER TABLE quotagate.decisions ADD CONSTRAINT %s CHECK (customer_id <> '%s') NOT VALID", constraint, customer.ID)
	if _, err := pool.Exec(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	drop := func() error {
		_, err := pool.Exec(context.Background(), "ALTER TABLE quotagate.decisions DROP CONSTRAINT IF EXISTS "+constraint)
		return err
	}
	t.Cleanup(func() {
		if err := drop(); err != nil {
			t.Errorf("remove injected constraint: %v", err)
		}
	})
	if _, err := s.DecideWithRate(ctx, customer.ID, id, "job.create", rateCheck(limiter, id)); err == nil {
		t.Fatal("injected PostgreSQL INSERT failure should fail closed")
	}
	assertCombinedCounts(t, pool, customer.ID, 0, 0, 0)
	assertRateUsed(t, raw, customer.ID, 1)
	if err := drop(); err != nil {
		t.Fatal(err)
	}
	allowed, err := s.DecideWithRate(ctx, customer.ID, id, "job.create", rateCheck(limiter, id))
	if err != nil || !allowed.Allowed {
		t.Fatalf("retry=%+v err=%v", allowed, err)
	}
	assertCombinedCounts(t, pool, customer.ID, 1, 1, 1)
	assertRateUsed(t, raw, customer.ID, 1)

	const dailyID = "combined_daily_000001"
	daily, err := s.DecideWithRate(ctx, customer.ID, dailyID, "job.create", rateCheck(limiter, dailyID))
	if err != nil || daily.Reason != "daily_quota" {
		t.Fatalf("daily denial=%+v err=%v", daily, err)
	}
	assertRateUsed(t, raw, customer.ID, 2)
	const rateID = "combined_rate_0000001"
	rate, err := s.DecideWithRate(ctx, customer.ID, rateID, "job.create", rateCheck(limiter, rateID))
	if err != nil || rate.Reason != "rate_limited" {
		t.Fatalf("rate denial=%+v err=%v", rate, err)
	}
	assertCombinedCounts(t, pool, customer.ID, 1, 3, 1)
	assertRateUsed(t, raw, customer.ID, 2)

	if _, err := s.PutPolicy(ctx, Policy{customer.ID, "job.create", 10, 10, 0}); err != nil {
		t.Fatal(err)
	}
	redisUnavailable := func(context.Context, Policy) (bool, error) {
		t.Error("a durable replay contacted Redis")
		return false, errors.New("Redis unavailable")
	}
	for replayID, want := range map[string]Decision{id: allowed, dailyID: daily, rateID: rate} {
		got, err := s.DecideWithRate(ctx, customer.ID, replayID, "job.create", redisUnavailable)
		if err != nil || got != want {
			t.Fatalf("replay=%+v want=%+v err=%v", got, want, err)
		}
	}
	if _, err := s.DecideWithRate(ctx, customer.ID, id, "job.other", redisUnavailable); !errors.Is(err, ErrDecisionConflict) {
		t.Fatalf("different operation: %v", err)
	}
	_, err = s.DecideWithRate(ctx, customer.ID, "new_redis_outage_001", "job.create", func(context.Context, Policy) (bool, error) {
		return false, errors.New("Redis unavailable")
	})
	if err == nil {
		t.Fatal("new decision during Redis outage should fail")
	}
	assertCombinedCounts(t, pool, customer.ID, 1, 3, 1)
}

func TestCombinedRateDenialWithoutDailyRowAndPreResultConflict(t *testing.T) {
	s, pool := testStore(t)
	limiter, raw := combinedRedis(t)
	customer := combinedCustomer(t, s, pool, raw, 10, 1)
	safeCombinedWindow(t, raw)
	ctx := context.Background()
	// An unfinished pre-acceptance uses the only rate slot, without daily usage.
	if _, err := limiter.Check(ctx, customer.ID, "unfinished_pre_00001", "job.create", 1); err != nil {
		t.Fatal(err)
	}
	const id = "denial_no_usage_0001"
	got, err := s.DecideWithRate(ctx, customer.ID, id, "job.create", rateCheck(limiter, id))
	if err != nil || got.Reason != "rate_limited" || got.DailyUsed != 0 {
		t.Fatalf("denial=%+v err=%v", got, err)
	}
	assertCombinedCounts(t, pool, customer.ID, 0, 1, 0)
	var rows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM quotagate.daily_usage WHERE customer_id=$1", customer.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rate denial created daily row: rows=%d err=%v", rows, err)
	}
	if _, err := s.PutPolicy(ctx, Policy{customer.ID, "job.other", 10, 1, 0}); err != nil {
		t.Fatal(err)
	}
	_, err = s.DecideWithRate(ctx, customer.ID, "unfinished_pre_00001", "job.other", rateCheck(limiter, "unfinished_pre_00001"))
	if !errors.Is(err, ErrDecisionConflict) {
		t.Fatalf("Redis pre-result conflict=%v", err)
	}
	assertCombinedCounts(t, pool, customer.ID, 0, 1, 0)
}
