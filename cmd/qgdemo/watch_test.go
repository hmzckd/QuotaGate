package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatcherWaitsForBothCopies(t *testing.T) {
	var status [2]atomic.Int64
	var origins [2]string
	for i := range 2 {
		status[i].Store(http.StatusOK)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(int(status[i].Load()))
		}))
		defer server.Close()
		origins[i] = server.URL
	}
	w := newWatcher(origins)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.run(ctx)
	if _, err := w.waitBoth(ctx, http.StatusOK, 2*time.Second); err != nil {
		t.Fatal(err)
	}

	status[0].Store(http.StatusServiceUnavailable)
	if _, err := w.waitBoth(ctx, http.StatusServiceUnavailable, 600*time.Millisecond); err == nil {
		t.Fatal("waitBoth returned while one copy was still ready")
	}
	status[1].Store(http.StatusServiceUnavailable)
	second := time.Now()
	at, err := w.waitBoth(ctx, http.StatusServiceUnavailable, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if at.Before(second) {
		t.Fatalf("both unready at %s, before the second copy changed at %s", at, second)
	}
	var changes []int
	for _, c := range w.history() {
		changes = append(changes, c.Copy*1000+c.Status)
	}
	if len(changes) != 4 {
		t.Fatalf("changes = %v, want 200 then 503 for each copy", changes)
	}
}
