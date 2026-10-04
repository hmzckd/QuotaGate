package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"quotagate/internal/ratelimit"
	"quotagate/internal/store"
)

type interruptedRate struct {
	RateLimiter
	failed bool
	calls  int
}

func (r *interruptedRate) Check(ctx context.Context, customerID, decisionID, operation string, limit int64) (ratelimit.Result, error) {
	r.calls++
	if r.failed {
		return ratelimit.Result{}, errors.New("private injected Redis error")
	}
	return r.RateLimiter.Check(ctx, customerID, decisionID, operation, limit)
}

func TestCombinedHTTPResponseLossAndFailClosed(t *testing.T) {
	dsn, redisURL := os.Getenv("QG_TEST_DATABASE_URL"), os.Getenv("QG_TEST_REDIS_URL")
	if dsn == "" || redisURL == "" {
		t.Skip("set both integration dependency URLs")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	limiter, err := ratelimit.New(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = limiter.Close() })
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	raw := redis.NewClient(opts)
	t.Cleanup(func() { _ = raw.Close() })
	s := store.New(pool)
	customer, key, err := s.CreateCustomer(ctx, "Combined HTTP test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		var cursor uint64
		for {
			keys, next, err := raw.Scan(cleanupCtx, cursor, fmt.Sprintf("qg:rate:%x:*", sha256.Sum256([]byte(customer.ID))), 100).Result()
			if err != nil {
				t.Errorf("scan test Redis keys: %v", err)
				break
			}
			if len(keys) > 0 {
				if err := raw.Del(cleanupCtx, keys...).Err(); err != nil {
					t.Errorf("delete test Redis keys: %v", err)
				}
			}
			cursor = next
			if cursor == 0 {
				break
			}
		}
		if _, err := pool.Exec(cleanupCtx, "DELETE FROM quotagate.customers WHERE id=$1", customer.ID); err != nil {
			t.Errorf("delete test customer: %v", err)
		}
	})
	if _, err := s.PutPolicy(ctx, store.Policy{CustomerID: customer.ID, Operation: "job.create", DailyLimit: 1, RateLimitPerMinute: 2}); err != nil {
		t.Fatal(err)
	}
	now, err := raw.Time(ctx).Result()
	if err != nil {
		t.Fatal(err)
	}
	remaining := now.Truncate(time.Minute).Add(time.Minute).Sub(now)
	if remaining < 10*time.Second {
		time.Sleep(remaining + 20*time.Millisecond)
	}
	rate := &interruptedRate{RateLimiter: limiter}
	handler := NewCombinedAppHandler(pool, s, strings.Repeat("b", 40), rate)
	call := func(id, operation, query, token, extra string, want int) store.Decision {
		t.Helper()
		body := fmt.Sprintf(`{"decision_id":%q,"operation":%q%s}`, id, operation, extra)
		r := httptest.NewRequest("POST", "/v1/decisions"+query, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("status=%d want=%d", w.Code, want)
		}
		if strings.Contains(w.Body.String(), "private") {
			t.Fatal("dependency error leaked")
		}
		var result store.Decision
		if want == 200 {
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("decision must not be cached")
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	const id = "combined_lost_response_001"
	lost := httptest.NewRequest("POST", "/v1/decisions", strings.NewReader(`{"decision_id":"`+id+`","operation":"job.create"}`))
	lost.Header.Set("Authorization", "Bearer "+key)
	handler.ServeHTTP(&lostResponse{header: make(http.Header)}, lost)
	if rate.calls != 1 {
		t.Fatalf("first rate calls=%d", rate.calls)
	}
	rate.failed = true
	replay := call(id, "job.create", "", key, "", 200)
	if !replay.Allowed || replay.DailyUsed != 1 || rate.calls != 1 {
		t.Fatal("committed replay used Redis or changed usage")
	}
	call(id, "job.other", "", key, "", 409)
	call("combined_outage_00001", "job.create", "", key, "", 503)
	usage, err := s.Usage(ctx, customer.ID, "job.create")
	if err != nil || usage.DailyUsed != 1 {
		t.Fatalf("outage changed usage: %+v %v", usage, err)
	}
	rate.failed = false
	daily := call("combined_daily_000001", "job.create", "", key, "", 200)
	rateDenied := call("combined_rate_0000001", "job.create", "", key, "", 200)
	if daily.Reason != "daily_quota" || rateDenied.Reason != "rate_limited" {
		t.Fatalf("denials: %+v %+v", daily, rateDenied)
	}
	if _, err := s.PutPolicy(ctx, store.Policy{CustomerID: customer.ID, Operation: "job.create", DailyLimit: 10, RateLimitPerMinute: 10}); err != nil {
		t.Fatal(err)
	}
	for savedID, want := range map[string]store.Decision{id: replay, "combined_daily_000001": daily, "combined_rate_0000001": rateDenied} {
		calls := rate.calls
		if got := call(savedID, "job.create", "", key, "", 200); got != want || rate.calls != calls {
			t.Fatal("policy update changed saved result or replay used Redis")
		}
	}
	fresh := call("combined_policy_v2_001", "job.create", "", key, "", 200)
	if !fresh.Allowed || fresh.PolicyVersion != 2 || fresh.DailyLimit != 10 {
		t.Fatalf("fresh policy=%+v", fresh)
	}
	call("combined_unknown_0001", "job.other", "", key, "", 403)
	call("combined_bad_auth_001", "job.create", "", "wrong", "", 401)
	call("combined_foreign_0001", "job.create", "", key, `,"customer_id":"foreign"`, 400)
	call("combined_foreign_0002", "job.create", "?customer_id=foreign", key, "", 400)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/demo/rate-checks", nil))
	if w.Code != 404 {
		t.Fatal("combined mode exposed a preliminary decision endpoint")
	}
	var used, records, allowed int
	if err := pool.QueryRow(ctx, `SELECT (SELECT sum(used) FROM quotagate.daily_usage WHERE customer_id=$1),
		count(*),count(*) FILTER (WHERE allowed) FROM quotagate.decisions WHERE customer_id=$1`, customer.ID).Scan(&used, &records, &allowed); err != nil || used != 2 || records != 4 || allowed != 2 {
		t.Fatalf("reconciliation=%d/%d/%d err=%v", used, records, allowed, err)
	}
}
