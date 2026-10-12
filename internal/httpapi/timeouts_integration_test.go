package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"quotagate/internal/store"
)

// A held row lock makes PostgreSQL hang for one customer, like a stuck
// dependency. The decision must end by its deadline or when the caller leaves,
// and neither the pool nor PostgreSQL may keep working on it afterwards.
func TestHungPostgresDecisionEndsAndReleasesConnections(t *testing.T) {
	dsn := os.Getenv("QG_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set QG_TEST_DATABASE_URL to run the PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := store.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s := store.New(pool)
	customer, key, err := s.CreateCustomer(ctx, "Timeout test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM quotagate.customers WHERE id = $1`, customer.ID)
	})
	if _, err := s.PutPolicy(ctx, store.Policy{CustomerID: customer.ID, Operation: "job.create", DailyLimit: 10, RateLimitPerMinute: 10}); err != nil {
		t.Fatal(err)
	}
	handler := NewAppHandler(pool, s, strings.Repeat("a", 40))
	decide := func(ctx context.Context, id string) (int, time.Duration) {
		r := httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(`{"decision_id":"`+id+`","operation":"job.create"}`)).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		start := time.Now()
		handler.ServeHTTP(w, r)
		return w.Code, time.Since(start)
	}
	waitingBackends := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%FOR UPDATE%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	blocker, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close(context.Background())
	lock, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(ctx, `SELECT id FROM quotagate.customers WHERE id = $1 FOR UPDATE`, customer.ID); err != nil {
		t.Fatal(err)
	}

	// 1. Nobody cancels: the 3s per-call deadline ends the wait with a 503.
	if code, elapsed := decide(context.Background(), "timeout_deadline_0001"); code != http.StatusServiceUnavailable || elapsed < 2500*time.Millisecond || elapsed > requestBudget+500*time.Millisecond {
		t.Fatalf("deadline: status %d after %s", code, elapsed)
	}

	// 2. The caller leaves after 200ms: the handler stops at once.
	callerCtx, leave := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, leave)
	if _, elapsed := decide(callerCtx, "timeout_canceled_001"); elapsed > time.Second {
		t.Fatalf("canceled request ran for %s", elapsed)
	}

	// 3. No pool connection stays checked out for the abandoned requests.
	deadline := time.Now().Add(2 * time.Second)
	for pool.Stat().AcquiredConns() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d connections still acquired", pool.Stat().AcquiredConns())
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 4. PostgreSQL stops waiting too, although the lock is still held: pgx
	// drops a connection whose context ended instead of reusing it.
	deadline = time.Now().Add(7 * time.Second)
	for waitingBackends() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d PostgreSQL backends still wait for the lock", waitingBackends())
		}
		time.Sleep(200 * time.Millisecond)
	}

	// 5. Once the dependency recovers, the same customer gets decisions again.
	if err := lock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if code, _ := decide(context.Background(), "timeout_recovered_01"); code != http.StatusOK {
		t.Fatalf("after recovery: status %d", code)
	}
}
