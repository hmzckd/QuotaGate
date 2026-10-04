package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("QG_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set QG_TEST_DATABASE_URL for PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return New(pool), pool
}

func testCustomer(t *testing.T, s *Store, pool *pgxpool.Pool, limit int64) Customer {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	customer, _, err := s.CreateCustomer(ctx, "Decision test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM quotagate.customers WHERE id = $1`, customer.ID); err != nil {
			t.Errorf("clean up customer: %v", err)
		}
	})
	if _, err := s.PutPolicy(ctx, Policy{CustomerID: customer.ID, Operation: "job.create", DailyLimit: limit, RateLimitPerMinute: 100}); err != nil {
		t.Fatal(err)
	}
	return customer
}

func TestConcurrentDecisionsNeverExceedDailyLimit(t *testing.T) {
	s, pool := testStore(t)
	customer := testCustomer(t, s, pool, 7)
	const requests = 40
	results := make(chan Decision, requests)
	errorsCh := make(chan error, requests)
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			decision, err := s.Decide(ctx, customer.ID, fmt.Sprintf("decision%024d", i), "job.create")
			if err != nil {
				errorsCh <- err
				return
			}
			results <- decision
		}(i)
	}
	wg.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		t.Errorf("concurrent decision: %v", err)
	}
	allowed, denied := 0, 0
	for result := range results {
		if result.Allowed && result.Reason == "allowed" {
			allowed++
		} else if !result.Allowed && result.Reason == "daily_quota" && result.DailyUsed == 7 {
			denied++
		} else {
			t.Errorf("unexpected decision: %+v", result)
		}
	}
	if allowed != 7 || denied != requests-7 {
		t.Fatalf("allowed=%d denied=%d, want 7/%d", allowed, denied, requests-7)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	usage, err := s.Usage(ctx, customer.ID, "job.create")
	if err != nil {
		t.Fatal(err)
	}
	if usage.DailyUsed != 7 {
		t.Fatalf("daily used=%d, want 7", usage.DailyUsed)
	}
	var recorded int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM quotagate.decisions WHERE customer_id = $1`, customer.ID).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != requests {
		t.Fatalf("recorded=%d, want %d", recorded, requests)
	}
}

func TestReplayAndPolicyChange(t *testing.T) {
	s, pool := testStore(t)
	customer := testCustomer(t, s, pool, 1)
	const duplicateID = "duplicate_decision_00001"
	results := make(chan Decision, 20)
	errorsCh := make(chan error, 20)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			decision, err := s.Decide(ctx, customer.ID, duplicateID, "job.create")
			if err != nil {
				errorsCh <- err
				return
			}
			results <- decision
		}()
	}
	wg.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		t.Errorf("duplicate decision: %v", err)
	}
	for result := range results {
		if !result.Allowed || result.DailyUsed != 1 || result.PolicyVersion != 1 {
			t.Errorf("duplicate changed result: %+v", result)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	denied, err := s.Decide(ctx, customer.ID, "denied_decision_00001", "job.create")
	if err != nil || denied.Allowed || denied.DailyUsed != 1 {
		t.Fatalf("expected quota denial: %+v, %v", denied, err)
	}
	policy, err := s.PutPolicy(ctx, Policy{CustomerID: customer.ID, Operation: "job.create", DailyLimit: 3, RateLimitPerMinute: 100})
	if err != nil || policy.Version != 2 {
		t.Fatalf("policy version: %+v, %v", policy, err)
	}
	firstReplay, err := s.Decide(ctx, customer.ID, duplicateID, "job.create")
	if err != nil || firstReplay.DailyLimit != 1 || firstReplay.PolicyVersion != 1 || !firstReplay.Allowed {
		t.Fatalf("allow replay changed after policy update: %+v, %v", firstReplay, err)
	}
	deniedReplay, err := s.Decide(ctx, customer.ID, "denied_decision_00001", "job.create")
	if err != nil || deniedReplay.Allowed || deniedReplay.PolicyVersion != 1 {
		t.Fatalf("deny replay changed after policy update: %+v, %v", deniedReplay, err)
	}
	if _, err := s.Decide(ctx, customer.ID, duplicateID, "other.operation"); !errors.Is(err, ErrDecisionConflict) {
		t.Fatalf("different operation with reused ID: %v", err)
	}
	newDecision, err := s.Decide(ctx, customer.ID, "fresh_decision_00001", "job.create")
	if err != nil || !newDecision.Allowed || newDecision.DailyUsed != 2 || newDecision.PolicyVersion != 2 {
		t.Fatalf("new policy was not applied: %+v, %v", newDecision, err)
	}
	usage, err := s.Usage(ctx, customer.ID, "job.create")
	if err != nil || usage.DailyUsed != 2 || usage.DailyLimit != 3 {
		t.Fatalf("replays consumed another right: %+v, %v", usage, err)
	}
}

func TestUTCMidnightUsesDatabaseClockBoundary(t *testing.T) {
	s, pool := testStore(t)
	customer := testCustomer(t, s, pool, 1)
	now := time.Date(2026, 10, 1, 23, 59, 59, 0, time.UTC)
	s.clock = func(context.Context, pgx.Tx) (time.Time, error) { return now, nil }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first, err := s.Decide(ctx, customer.ID, "day_one_decision_0001", "job.create")
	if err != nil || !first.Allowed || first.UTCDay != "2026-10-01" {
		t.Fatalf("day one: %+v, %v", first, err)
	}
	now = time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	second, err := s.Decide(ctx, customer.ID, "day_two_decision_0001", "job.create")
	if err != nil || !second.Allowed || second.UTCDay != "2026-10-02" || second.DailyUsed != 1 {
		t.Fatalf("day two: %+v, %v", second, err)
	}
	replay, err := s.Decide(ctx, customer.ID, "day_one_decision_0001", "job.create")
	if err != nil || replay.UTCDay != "2026-10-01" || replay.DailyUsed != 1 {
		t.Fatalf("cross-day replay: %+v, %v", replay, err)
	}
	usage, err := s.Usage(ctx, customer.ID, "job.create")
	if err != nil || usage.UTCDay != "2026-10-02" || usage.DailyUsed != 1 || usage.ResetsAtUTC != "2026-10-03T00:00:00Z" {
		t.Fatalf("usage at UTC midnight: %+v, %v", usage, err)
	}
	var days int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM quotagate.daily_usage WHERE customer_id = $1 AND used = 1`, customer.ID).Scan(&days); err != nil || days != 2 {
		t.Fatalf("daily rows=%d, error=%v", days, err)
	}
}
