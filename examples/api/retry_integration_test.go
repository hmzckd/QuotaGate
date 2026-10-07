package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"quotagate/client"
	"quotagate/internal/httpapi"
	"quotagate/internal/ratelimit"
	"quotagate/internal/store"
)

type retryDependencies struct {
	pool    *pgxpool.Pool
	redis   *redis.Client
	limiter *ratelimit.Limiter
}

func retryTestDependencies(t *testing.T) retryDependencies {
	t.Helper()
	dsn, redisURL := os.Getenv("QG_TEST_DATABASE_URL"), os.Getenv("QG_TEST_REDIS_URL")
	if dsn == "" || redisURL == "" {
		t.Skip("set both dependency URLs for real retry integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	options, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	raw := redis.NewClient(options)
	t.Cleanup(func() { _ = raw.Close() })
	limiter, err := ratelimit.New(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = limiter.Close() })
	return retryDependencies{pool, raw, limiter}
}

func TestExampleRetryAfterCommitWithPostgres(t *testing.T) {
	deps := retryTestDependencies(t)
	const admin = "local-retry-test-admin-token-0000000001"
	backend := httptest.NewServer(httpapi.NewCombinedAppHandler(deps.pool, store.New(deps.pool), admin, deps.limiter))
	t.Cleanup(backend.Close)
	runRetryAcceptance(t, deps, []string{backend.URL}, admin)
}

func TestQG07ComposeRetry(t *testing.T) {
	origin, second, admin := os.Getenv("QG_TEST_API_URL"), os.Getenv("QG_TEST_API_URL_2"), os.Getenv("QG_TEST_ADMIN_TOKEN")
	if origin == "" || second == "" || admin == "" {
		t.Skip("set isolated Compose API URLs and admin token for live acceptance")
	}
	deps := retryTestDependencies(t)
	runRetryAcceptance(t, deps, []string{origin, second}, admin)
}

func runRetryAcceptance(t *testing.T, deps retryDependencies, origins []string, admin string) {
	t.Helper()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	backendHTTP := &http.Client{Transport: transport, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(transport.CloseIdleConnections)
	callBackend := func(method, path, key string, body []byte) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, method, origins[0]+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := backendHTTP.Do(req)
		if err != nil {
			t.Fatal("backend request unavailable")
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 && resp.StatusCode != 201 {
			t.Fatalf("backend status=%d", resp.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
		if err != nil || len(data) > 4096 {
			t.Fatal("backend response unavailable")
		}
		return data
	}
	create := func(name string, daily int64) (string, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"name": name})
		var customer struct {
			ID  string `json:"id"`
			Key string `json:"api_key"`
		}
		if err := json.Unmarshal(callBackend("POST", "/v1/admin/customers", admin, body), &customer); err != nil || !strings.HasPrefix(customer.ID, "cus_") || len(customer.Key) != 67 {
			t.Fatal("invalid test customer")
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var cursor uint64
			for {
				keys, next, err := deps.redis.Scan(ctx, cursor, fmt.Sprintf("qg:rate:%x:*", sha256.Sum256([]byte(customer.ID))), 100).Result()
				if err != nil {
					t.Errorf("Redis cleanup: %v", err)
					break
				}
				if len(keys) > 0 {
					if err := deps.redis.Del(ctx, keys...).Err(); err != nil {
						t.Errorf("Redis cleanup: %v", err)
					}
				}
				cursor = next
				if cursor == 0 {
					break
				}
			}
			if _, err := deps.pool.Exec(ctx, "DELETE FROM quotagate.customers WHERE id=$1", customer.ID); err != nil {
				t.Errorf("customer cleanup: %v", err)
			}
		})
		policy, _ := json.Marshal(map[string]int64{"daily_limit": daily, "rate_limit_per_minute": 1000})
		callBackend("PUT", "/v1/admin/customers/"+customer.ID+"/policies/job.create", admin, policy)
		return customer.ID, customer.Key
	}

	// The proxy is test-only. It reads the real final response before disconnecting
	// or stalling, so every fault below occurs after PostgreSQL commit.
	type trace struct {
		ID, Body string
		Decision client.Decision
	}
	type faultProxy struct {
		mode     atomic.Uint32 // 0=normal, 1=lose first, 2=lose every reply, 3=stall after commit
		mu       sync.Mutex
		attempts map[string]int
		traces   []trace
	}
	newExample := func(key string) (http.Handler, *faultProxy) {
		t.Helper()
		fault := &faultProxy{attempts: map[string]int{}}
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
			if err != nil {
				t.Error("proxy request read failed")
				w.WriteHeader(503)
				return
			}
			var input struct {
				ID string `json:"decision_id"`
			}
			if err := json.Unmarshal(body, &input); err != nil || len(input.ID) != 32 || r.Header.Get("Authorization") != "Bearer "+key {
				t.Error("proxy identity mismatch")
				w.WriteHeader(400)
				return
			}
			fault.mu.Lock()
			index := len(fault.traces)
			fault.mu.Unlock()
			req, err := http.NewRequestWithContext(r.Context(), "POST", origins[index%len(origins)]+"/v1/decisions", bytes.NewReader(body))
			if err != nil {
				t.Error(err)
				w.WriteHeader(503)
				return
			}
			req.Header.Set("Authorization", r.Header.Get("Authorization"))
			req.Header.Set("Content-Type", "application/json")
			resp, err := backendHTTP.Do(req)
			if err != nil {
				t.Error("proxy backend unavailable")
				w.WriteHeader(503)
				return
			}
			data, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
			_ = resp.Body.Close()
			if err != nil || resp.StatusCode != 200 {
				t.Error("proxy backend did not return a final decision")
				w.WriteHeader(503)
				return
			}
			var decision client.Decision
			if err := json.Unmarshal(data, &decision); err != nil {
				t.Error("invalid backend decision")
				w.WriteHeader(503)
				return
			}
			fault.mu.Lock()
			fault.attempts[input.ID]++
			attempt := fault.attempts[input.ID]
			fault.traces = append(fault.traces, trace{input.ID, string(body), decision})
			fault.mu.Unlock()
			switch fault.mode.Load() {
			case 1:
				if attempt != 1 {
					break
				}
				fallthrough
			case 2:
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
				return
			case 3:
				<-r.Context().Done()
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(data)
		}))
		t.Cleanup(proxy.Close)
		decisions, err := client.New(proxy.URL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(decisions.Close)
		return newHandler(decisions), fault
	}
	callExample := func(handler http.Handler, key string, ctx context.Context, want int) {
		t.Helper()
		r := httptest.NewRequest("POST", "/jobs", strings.NewReader(`{"payload":"same inbound body"}`)).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Idempotency-Key", "same-inbound-header")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("example status=%d want=%d", w.Code, want)
		}
		if strings.Contains(w.Body.String(), key) || strings.Contains(w.Body.String(), admin) {
			t.Fatal("credential leaked to example response")
		}
	}
	assertHandlers := func(handler http.Handler, want int64) {
		t.Helper()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "/demo/stats", nil))
		var stats struct {
			Calls int64 `json:"handler_calls"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil || stats.Calls != want {
			t.Fatalf("handler calls=%d want=%d", stats.Calls, want)
		}
	}
	assertDurable := func(customerID string, used, records, allowed, rate int64) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var gotUsed, gotRecords, gotAllowed int64
		if err := deps.pool.QueryRow(ctx, `SELECT COALESCE((SELECT sum(used) FROM quotagate.daily_usage WHERE customer_id=$1),0),count(*),count(*) FILTER(WHERE allowed) FROM quotagate.decisions WHERE customer_id=$1`, customerID).Scan(&gotUsed, &gotRecords, &gotAllowed); err != nil || gotUsed != used || gotRecords != records || gotAllowed != allowed {
			t.Fatalf("daily/records/allowed=%d/%d/%d want=%d/%d/%d err=%v", gotUsed, gotRecords, gotAllowed, used, records, allowed, err)
		}
		bucket := fmt.Sprintf("qg:rate:%x:bucket:%x", sha256.Sum256([]byte(customerID)), sha256.Sum256([]byte("job.create")))
		if got, err := deps.redis.HGet(ctx, bucket, "used").Int64(); err != nil || got != rate {
			t.Fatalf("Redis used=%d want=%d err=%v", got, rate, err)
		}
	}
	assertAttempts := func(fault *faultProxy, wantIDs, wantCalls int) []trace {
		t.Helper()
		fault.mu.Lock()
		defer fault.mu.Unlock()
		if len(fault.attempts) != wantIDs || len(fault.traces) != wantCalls {
			t.Fatalf("unique IDs=%d attempts=%d want=%d/%d", len(fault.attempts), len(fault.traces), wantIDs, wantCalls)
		}
		first := map[string]trace{}
		for _, item := range fault.traces {
			if saved, ok := first[item.ID]; ok && (saved.Body != item.Body || saved.Decision != item.Decision) {
				t.Fatal("retry changed decision body or committed result")
			}
			first[item.ID] = item
		}
		return append([]trace(nil), fault.traces...)
	}
	now, err := deps.redis.Time(context.Background()).Result()
	if err != nil {
		t.Fatal(err)
	}
	remaining := now.Truncate(time.Minute).Add(time.Minute).Sub(now)
	if remaining < 10*time.Second {
		time.Sleep(remaining + 20*time.Millisecond)
	}

	t.Run("commit response loss recovers and inbound IDs stay separate", func(t *testing.T) {
		id, key := create("QG-07 recovered replies", 2)
		handler, fault := newExample(key)
		fault.mode.Store(1)
		callExample(handler, key, context.Background(), 201)
		assertDurable(id, 1, 1, 1, 1)
		assertHandlers(handler, 1)
		callExample(handler, key, context.Background(), 201)
		assertDurable(id, 2, 2, 2, 2)
		assertHandlers(handler, 2)
		traces := assertAttempts(fault, 2, 4)
		if traces[0].ID != traces[1].ID || traces[2].ID != traces[3].ID || traces[0].ID == traces[2].ID {
			t.Fatal("transport/inbound identities crossed")
		}
		fault.mode.Store(0)
		callExample(handler, key, context.Background(), 429)
		assertDurable(id, 2, 3, 2, 3)
		assertHandlers(handler, 2)
		assertAttempts(fault, 3, 5)
		t.Log("PASS first reply lost after commit: retry recovers; two inbound requests=two decisions/handlers; daily denial blocks handler")
	})
	t.Run("all replies lost exhaust attempts and manual replay finds commit", func(t *testing.T) {
		id, key := create("QG-07 exhausted replies", 2)
		handler, fault := newExample(key)
		fault.mode.Store(2)
		callExample(handler, key, context.Background(), 503)
		assertDurable(id, 1, 1, 1, 1)
		assertHandlers(handler, 0)
		traces := assertAttempts(fault, 1, 3)
		body, _ := json.Marshal(map[string]string{"decision_id": traces[0].ID, "operation": "job.create"})
		var replay client.Decision
		if err := json.Unmarshal(callBackend("POST", "/v1/decisions", key, body), &replay); err != nil || replay != traces[0].Decision {
			t.Fatal("manual replay lost durable result")
		}
		assertDurable(id, 1, 1, 1, 1)
		assertHandlers(handler, 0)
		fault.mode.Store(1)
		callExample(handler, key, context.Background(), 201)
		assertDurable(id, 2, 2, 2, 2)
		assertHandlers(handler, 1)
		assertAttempts(fault, 2, 5)
		fault.mode.Store(0)
		callExample(handler, key, context.Background(), 429)
		assertDurable(id, 2, 3, 2, 3)
		assertHandlers(handler, 1)
		assertAttempts(fault, 3, 6)
		t.Log("PASS three lost replies=503 and zero handlers, one durable allowance; manual replay recovers it; new inbound request gets a fresh decision")
	})
	t.Run("caller deadline after commit blocks handler", func(t *testing.T) {
		id, key := create("QG-07 caller deadline", 1000)
		handler, fault := newExample(key)
		fault.mode.Store(3)
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		started := time.Now()
		callExample(handler, key, ctx, 503)
		if time.Since(started) > time.Second {
			t.Fatal("caller deadline was reset")
		}
		assertDurable(id, 1, 1, 1, 1)
		assertHandlers(handler, 0)
		assertAttempts(fault, 1, 1)
		t.Log("PASS caller deadline overrides retry budget; committed allowance can remain; handler blocked")
	})
	t.Run("one total budget covers every timed out attempt", func(t *testing.T) {
		id, key := create("QG-07 total timeout", 1000)
		handler, fault := newExample(key)
		fault.mode.Store(3)
		started := time.Now()
		callExample(handler, key, context.Background(), 503)
		elapsed := time.Since(started)
		if elapsed < 1500*time.Millisecond || elapsed > 2500*time.Millisecond {
			t.Fatalf("total retry budget elapsed=%s", elapsed)
		}
		assertDurable(id, 1, 1, 1, 1)
		assertHandlers(handler, 0)
		assertAttempts(fault, 1, 3)
		t.Logf("PASS three stalled replies under one 2s budget: elapsed=%s; daily/decision/Redis=1/1/1, handler=0", elapsed)
	})
}
