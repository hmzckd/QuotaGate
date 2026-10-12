package loadtest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestRunKeepsScheduleWhenResponsesAreSlow(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Header.Get("Authorization")]++
		mu.Unlock()
		time.Sleep(200 * time.Millisecond)
		if r.Header.Get("Authorization") == "Bearer key-b" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":"rate_limited"}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	plan := []int{0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 0}
	start := time.Now()
	results, err := Run(context.Background(), server.Client(), Target{URL: server.URL, Keys: []string{"key-a", "key-b"}}, plan, 50, 20)
	if err != nil {
		t.Fatal(err)
	}
	// 25 requests at 50/s take 0.5s to send; a closed loop waiting for each
	// 200ms response would need 25 × 200ms = 5s.
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("run took %s; slow responses delayed the schedule", elapsed)
	}
	if seen["Bearer key-a"] != 13 || seen["Bearer key-b"] != 12 {
		t.Fatalf("requests per key = %v", seen)
	}
	for _, r := range results {
		if want := []int{201, 429}[r.Customer]; r.Status != want || (r.Customer == 1 && r.Reason != "rate_limited") {
			t.Fatalf("customer %d got %d %q", r.Customer, r.Status, r.Reason)
		}
	}
}

func TestRunCountsWaitForAFreeWorker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	// One worker, a request every 10ms, 50ms per response: requests queue up.
	results, err := Run(context.Background(), server.Client(), Target{URL: server.URL, Keys: []string{"k"}}, make([]int, 10), 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	// The last one is scheduled at 90ms but can start only after nine 50ms
	// responses (450ms). Measuring from the send would hide that wait.
	if last := results[9]; last.Latency < 350*time.Millisecond || last.Done.Sub(last.Sent) > 200*time.Millisecond {
		t.Fatalf("last latency %s, own time %s", last.Latency, last.Done.Sub(last.Sent))
	}
}

func TestRunStopsWhenCanceled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Run(ctx, server.Client(), Target{URL: server.URL, Keys: []string{"k"}}, make([]int, 1000), 10, 2)
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("err = %v after %s; want a prompt cancel", err, time.Since(start))
	}
}
