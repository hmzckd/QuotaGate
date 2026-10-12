package loadtest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

type Result struct {
	Customer int
	// Status is the HTTP status, or 0 when no response arrived.
	Status int
	// Reason is the 429 body's error field: daily_quota or rate_limited.
	Reason string
	// Latency runs from the scheduled send time to the end of the response,
	// so time spent waiting for a free worker counts too.
	Latency time.Duration
	// Sent and Done are wall-clock times of the actual send and the response.
	Sent, Done time.Time
}

// Target is one example API: the POST /jobs URL and the API key per customer.
type Target struct {
	URL  string
	Keys []string
}

// Run sends request i at start + i/rate. It is open loop: a slow response
// does not push later requests back, like real users who do not wait for each
// other. At most concurrency requests are in flight at once.
func Run(ctx context.Context, client *http.Client, target Target, plan []int, rate float64, concurrency int) ([]Result, error) {
	results := make([]Result, len(plan))
	interval := time.Duration(float64(time.Second) / rate)
	start := time.Now()
	scheduled := func(i int) time.Time { return start.Add(time.Duration(i) * interval) }

	// Buffered for the whole plan: the scheduler never blocks on busy workers.
	jobs := make(chan int, len(plan))
	var wg sync.WaitGroup
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i] = send(ctx, client, target, plan[i], scheduled(i))
			}
		}()
	}

	timer := time.NewTimer(0)
	defer timer.Stop()
	var err error
	for i := range plan {
		timer.Reset(time.Until(scheduled(i)))
		select {
		case <-timer.C:
		case <-ctx.Done():
			err = ctx.Err()
		}
		if err != nil {
			break
		}
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return results, err
}

func send(ctx context.Context, client *http.Client, target Target, customer int, scheduled time.Time) Result {
	result := Result{Customer: customer, Sent: time.Now()}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, nil)
	if err == nil {
		req.Header.Set("Authorization", "Bearer "+target.Keys[customer])
		var resp *http.Response
		if resp, err = client.Do(req); err == nil {
			result.Status = resp.StatusCode
			if resp.StatusCode == http.StatusTooManyRequests {
				var body struct {
					Error string `json:"error"`
				}
				_ = json.NewDecoder(io.LimitReader(resp.Body, 1024)).Decode(&body)
				result.Reason = body.Error
			}
			// Read to the end so the connection can be reused.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
		}
	}
	result.Done = time.Now()
	result.Latency = result.Done.Sub(scheduled)
	return result
}
