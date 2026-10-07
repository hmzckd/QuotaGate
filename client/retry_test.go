package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func closeReply(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Error(err)
		return
	}
	_ = conn.Close()
}

func TestMiddlewareRetriesPreserveIDBodyAndCredentials(t *testing.T) {
	for _, fault := range []string{"disconnect", "truncated body", "attempt timeout", "502", "503", "504"} {
		t.Run(fault, func(t *testing.T) {
			var mu sync.Mutex
			var bodies, keys []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				bodies = append(bodies, string(body))
				keys = append(keys, r.Header.Get("Authorization"))
				first := len(bodies) == 1
				mu.Unlock()
				if first {
					switch fault {
					case "disconnect":
						closeReply(t, w)
					case "truncated body":
						w.Header().Set("Content-Length", "1000")
						_, _ = io.WriteString(w, `{"allowed":`)
						w.(http.Flusher).Flush()
						closeReply(t, w)
					case "attempt timeout":
						<-r.Context().Done()
					default:
						status := map[string]int{"502": 502, "503": 503, "504": 504}[fault]
						w.WriteHeader(status)
						_, _ = io.WriteString(w, "private upstream error")
					}
					return
				}
				_, _ = io.WriteString(w, allowedBody)
			}))
			t.Cleanup(upstream.Close)
			c := testClient(t, upstream.URL)
			c.attemptTimeout, c.retryDelay = 40*time.Millisecond, time.Millisecond
			var handlers atomic.Int64
			handler := c.Middleware("job.create", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handlers.Add(1)
				body, _ := io.ReadAll(r.Body)
				if string(body) != "original job body" {
					t.Error("retry changed inbound body")
				}
				w.WriteHeader(201)
			}))
			r := httptest.NewRequest("POST", "/jobs", strings.NewReader("original job body"))
			r.Header.Set("Authorization", "Bearer "+testKey)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			mu.Lock()
			defer mu.Unlock()
			if w.Code != 201 || handlers.Load() != 1 || len(bodies) != 2 || bodies[0] != bodies[1] || keys[0] != "Bearer "+testKey || keys[1] != keys[0] {
				t.Fatalf("status=%d handlers=%d attempts=%d", w.Code, handlers.Load(), len(bodies))
			}
			var wire struct {
				DecisionID string `json:"decision_id"`
				Operation  string `json:"operation"`
			}
			if err := json.Unmarshal([]byte(bodies[0]), &wire); err != nil || len(wire.DecisionID) != 32 || wire.Operation != "job.create" {
				t.Fatal("invalid decision identity")
			}
		})
	}
}

func TestDecisionOverallBudgetIncludesRetryDelay(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)
	c := testClient(t, upstream.URL)
	c.timeout, c.attemptTimeout, c.retryDelay = 120*time.Millisecond, 80*time.Millisecond, 80*time.Millisecond
	started := time.Now()
	_, err := c.Decide(context.Background(), testKey, "budget_identity_0001", "job.create")
	elapsed := time.Since(started)
	if err == nil || calls.Load() != 1 || elapsed < 100*time.Millisecond || elapsed > 300*time.Millisecond {
		t.Fatalf("calls=%d elapsed=%s error=%v", calls.Load(), elapsed, err)
	}
}

func TestMiddlewareCallerDeadlineAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "short caller deadline", true: "canceled during backoff"}[canceled], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
			defer cancel()
			var calls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				if canceled {
					w.WriteHeader(503)
					cancel()
					return
				}
				<-r.Context().Done()
			}))
			t.Cleanup(upstream.Close)
			c := testClient(t, upstream.URL)
			handler := c.Middleware("job.create", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("handler ran after caller timeout/cancellation") }))
			r := httptest.NewRequest("POST", "/jobs", nil).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer "+testKey)
			started := time.Now()
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != 503 || calls.Load() != 1 || time.Since(started) > 300*time.Millisecond {
				t.Fatalf("status=%d calls=%d elapsed=%s", w.Code, calls.Load(), time.Since(started))
			}
		})
	}
}

func TestMiddlewareFreshIDsWithConcurrentRetries(t *testing.T) {
	var ids sync.Map
	var requests, handlers atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var wire struct {
			DecisionID string `json:"decision_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
			t.Error(err)
			return
		}
		entry, _ := ids.LoadOrStore(wire.DecisionID, &atomic.Int64{})
		requests.Add(1)
		if entry.(*atomic.Int64).Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		_, _ = io.WriteString(w, allowedBody)
	}))
	t.Cleanup(upstream.Close)
	c := testClient(t, upstream.URL)
	c.retryDelay = time.Millisecond
	handler := c.Middleware("job.create", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handlers.Add(1); w.WriteHeader(201) }))
	var wg sync.WaitGroup
	for range 30 {
		wg.Go(func() {
			r := httptest.NewRequest("POST", "/jobs", strings.NewReader("same payload"))
			r.Header.Set("Authorization", "Bearer "+testKey)
			r.Header.Set("Idempotency-Key", "same inbound header")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != 201 {
				t.Errorf("status=%d", w.Code)
			}
		})
	}
	wg.Wait()
	unique := 0
	ids.Range(func(_, value any) bool {
		unique++
		if value.(*atomic.Int64).Load() != 2 {
			t.Error("retry attempts crossed inbound identities")
		}
		return true
	})
	if unique != 30 || requests.Load() != 60 || handlers.Load() != 30 {
		t.Fatalf("unique=%d attempts=%d handlers=%d", unique, requests.Load(), handlers.Load())
	}
}

func TestTLSVerificationFailureIsTerminal(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("untrusted TLS request reached handler") }))
	t.Cleanup(upstream.Close)
	c := testClient(t, upstream.URL)
	var calls atomic.Int64
	c.http.Transport = countingTransport{base: c.transport, calls: &calls}
	if _, err := c.Decide(context.Background(), testKey, "tls_invalid_0000001", "job.create"); err == nil || calls.Load() != 1 {
		t.Fatalf("TLS attempts=%d error=%v", calls.Load(), err)
	}
}

type countingTransport struct {
	base  http.RoundTripper
	calls *atomic.Int64
}

func (t countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return t.base.RoundTrip(r)
}
