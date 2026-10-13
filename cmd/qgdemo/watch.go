package main

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// change is one readiness change of one QuotaGate copy.
type change struct {
	At     time.Time `json:"at"`
	Copy   int       `json:"copy"`
	Status int       `json:"status"` // 200, 503, or 0 when there was no answer
}

// watcher asks both copies GET /health/ready every 250ms during the load and
// keeps every change, so the report shows when each copy noticed the outage
// and the recovery.
type watcher struct {
	origins [2]string
	client  *http.Client

	mu      sync.Mutex
	status  [2]int
	since   [2]time.Time
	changes []change
}

// newWatcher waits up to 2s for each answer: the readiness check bounds its
// own dependency pings at 1s, so its 503 always arrives in time.
func newWatcher(origins [2]string) *watcher {
	return &watcher{origins: origins, client: &http.Client{Timeout: 2 * time.Second}}
}

// run polls both copies side by side, so a slow answer from one does not
// delay noticing the other, until ctx ends.
func (w *watcher) run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := range w.origins {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.poll(ctx, i)
		}()
	}
	wg.Wait()
}

func (w *watcher) poll(ctx context.Context, i int) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		status := 0
		if resp, err := w.client.Get(w.origins[i] + "/health/ready"); err == nil {
			status = resp.StatusCode
			resp.Body.Close()
		}
		w.mu.Lock()
		if status != w.status[i] {
			now := time.Now()
			w.status[i], w.since[i] = status, now
			w.changes = append(w.changes, change{At: now, Copy: i + 1, Status: status})
		}
		w.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// waitBoth waits until both copies answer want and returns when the later of
// the two started answering it.
func (w *watcher) waitBoth(ctx context.Context, want int, timeout time.Duration) (time.Time, error) {
	deadline := time.Now().Add(timeout)
	for {
		w.mu.Lock()
		both, at := w.status[0] == want && w.status[1] == want, w.since[0]
		if w.since[1].After(at) {
			at = w.since[1]
		}
		w.mu.Unlock()
		if both {
			return at, nil
		}
		if time.Now().After(deadline) {
			return time.Time{}, fmt.Errorf("both QuotaGate copies did not answer /health/ready with %d within %s", want, timeout)
		}
		select {
		case <-ctx.Done():
			return time.Time{}, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (w *watcher) history() []change {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]change(nil), w.changes...)
}
