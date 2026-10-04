package client

import (
	"encoding/hex"
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

const testKey = "qg_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const allowedBody = `{"allowed":true,"reason":"allowed","utc_day":"2026-10-03","daily_used":1,"daily_limit":2,"policy_version":1}`
const deniedBody = `{"allowed":false,"reason":"daily_quota","utc_day":"2026-10-03","daily_used":2,"daily_limit":2,"policy_version":1}`

func testClient(t *testing.T, origin string) *Client {
	t.Helper()
	c, err := New(origin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestMiddlewareDecisionOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{"allowed", 200, allowedBody, 201},
		{"daily quota", 200, deniedBody, 429},
		{"rate limit", 200, strings.Replace(deniedBody, "daily_quota", "rate_limited", 1), 429},
		{"invalid key", 401, `{"error":"secret from upstream"}`, 401},
		{"missing policy", 403, `{"error":"secret from upstream"}`, 403},
		{"bad client request", 400, `{"error":"secret from upstream"}`, 503},
		{"conflict", 409, `{"error":"secret from upstream"}`, 503},
		{"storage outage", 503, `{"error":"secret from upstream"}`, 503},
		{"server error", 500, "secret from upstream", 503},
		{"malformed decision", 200, `{"allowed":true`, 503},
		{"missing permission", 200, strings.Replace(allowedBody, `"allowed":true,`, "", 1), 503},
		{"missing limits", 200, `{"allowed":true,"reason":"allowed"}`, 503},
		{"contradictory permission", 200, strings.Replace(allowedBody, `"reason":"allowed"`, `"reason":"daily_quota"`, 1), 503},
		{"invalid day", 200, strings.Replace(allowedBody, "2026-10-03", "2026-13-03", 1), 503},
		{"unknown rejection", 200, strings.Replace(deniedBody, "daily_quota", "unknown", 1), 503},
		{"multiple objects", 200, allowedBody + allowedBody, 503},
		{"oversized decision", 200, allowedBody + strings.Repeat(" ", 4096), 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var upstreamCalls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				if r.Method != "POST" || r.URL.Path != "/v1/decisions" || r.Header.Get("Authorization") != "Bearer "+testKey {
					t.Error("wrong decision request route or credentials")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(upstream.Close)
			var handlerCalls int
			c := testClient(t, upstream.URL)
			handler := c.Middleware("job.create", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handlerCalls++
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != "original job body" {
					t.Error("middleware consumed or changed the original request body")
				}
				w.WriteHeader(http.StatusCreated)
			}))
			r := httptest.NewRequest("POST", "/jobs", strings.NewReader("original job body"))
			r.Header.Set("Authorization", "Bearer "+testKey)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
			wantCalls := 0
			if tc.want == http.StatusCreated {
				wantCalls = 1
			}
			if handlerCalls != wantCalls || upstreamCalls.Load() != 1 {
				t.Fatalf("handler calls = %d, upstream calls = %d", handlerCalls, upstreamCalls.Load())
			}
			if strings.Contains(w.Body.String(), "secret from upstream") || strings.Contains(w.Body.String(), testKey) {
				t.Fatal("upstream error or credential exposed to caller")
			}
		})
	}
}

func TestMiddlewareFreshIDsUnderConcurrency(t *testing.T) {
	var ids sync.Map
	var decisions, handlerCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			DecisionID string `json:"decision_id"`
			Operation  string `json:"operation"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		decoded, err := hex.DecodeString(body.DecisionID)
		if err != nil || len(decoded) != 16 || body.Operation != "job.create" {
			t.Error("invalid generated decision ID or operation")
		}
		if _, exists := ids.LoadOrStore(body.DecisionID, true); exists {
			t.Error("separate inbound requests reused a decision ID")
		}
		decisions.Add(1)
		_, _ = io.WriteString(w, allowedBody)
	}))
	t.Cleanup(upstream.Close)
	c := testClient(t, upstream.URL)
	handler := c.Middleware("job.create", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalls.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	var wg sync.WaitGroup
	for range 30 {
		wg.Go(func() {
			r := httptest.NewRequest("POST", "/jobs", strings.NewReader(`{"decision_id":"caller_supplied_id"}`))
			r.Header.Set("Authorization", "Bearer "+testKey)
			r.Header.Set("Idempotency-Key", "same-caller-key")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusCreated {
				t.Errorf("status = %d, want 201", w.Code)
			}
		})
	}
	wg.Wait()
	if decisions.Load() != 30 || handlerCalls.Load() != 30 {
		t.Fatalf("decisions = %d, handler calls = %d", decisions.Load(), handlerCalls.Load())
	}
}

func TestMiddlewareUnavailableAndTimeout(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "unreachable", true: "timeout"}[timeout], func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				<-r.Context().Done()
			}))
			c := testClient(t, upstream.URL)
			if timeout {
				c.http.Timeout = 50 * time.Millisecond
				t.Cleanup(upstream.Close)
			} else {
				upstream.Close()
			}
			handler := c.Middleware("job.create", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("handler ran without a decision")
			}))
			r := httptest.NewRequest("POST", "/jobs", nil)
			r.Header.Set("Authorization", "Bearer "+testKey)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", w.Code)
			}
		})
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	var redirectedCalls atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedCalls.Add(1)
		_, _ = io.WriteString(w, allowedBody)
	}))
	t.Cleanup(target.Close)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(upstream.Close)
	c := testClient(t, upstream.URL)
	handler := c.Middleware("job.create", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("handler ran after an upstream redirect")
	}))
	r := httptest.NewRequest("POST", "/jobs", nil)
	r.Header.Set("Authorization", "Bearer "+testKey)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 503 || redirectedCalls.Load() != 0 {
		t.Fatalf("status = %d, redirected calls = %d", w.Code, redirectedCalls.Load())
	}
}

func TestMiddlewareRejectsAmbiguousCredentials(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("invalid local credentials reached QuotaGate")
	}))
	t.Cleanup(upstream.Close)
	c := testClient(t, upstream.URL)
	handler := c.Middleware("job.create", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("unauthenticated handler ran")
	}))
	for _, values := range [][]string{nil, {"Bearer wrong"}, {"Bearer " + testKey, "Bearer " + testKey}} {
		r := httptest.NewRequest("POST", "/jobs", nil)
		for _, value := range values {
			r.Header.Add("Authorization", value)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Fatalf("invalid credentials status = %d", w.Code)
		}
	}
}
