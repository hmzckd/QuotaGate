package loadtest

import (
	"math"
	"net/http"
	"slices"
	"time"
)

type Summary struct {
	Requests int `json:"requests"`
	// Statuses counts responses by HTTP status; 0 means no response arrived.
	Statuses map[int]int    `json:"statuses"`
	Reasons  map[string]int `json:"reasons"`
	// Errors counts everything except 201 (allowed) and 429 (a limit's answer).
	Errors int           `json:"errors"`
	P50    time.Duration `json:"p50"`
	P95    time.Duration `json:"p95"`
	P99    time.Duration `json:"p99"`
	Max    time.Duration `json:"max"`
	// Rate is the achieved request rate from the first send to the last response.
	Rate float64 `json:"rate"`
}

func Summarize(results []Result) Summary {
	s := Summary{Requests: len(results), Statuses: map[int]int{}, Reasons: map[string]int{}}
	if len(results) == 0 {
		return s
	}
	latencies := make([]time.Duration, len(results))
	first, last := results[0].Sent, results[0].Done
	for i, r := range results {
		latencies[i] = r.Latency
		s.Statuses[r.Status]++
		if r.Status == http.StatusTooManyRequests {
			s.Reasons[r.Reason]++
		}
		if r.Status != http.StatusCreated && r.Status != http.StatusTooManyRequests {
			s.Errors++
		}
		if r.Sent.Before(first) {
			first = r.Sent
		}
		if r.Done.After(last) {
			last = r.Done
		}
	}
	slices.Sort(latencies)
	s.P50, s.P95, s.P99 = Percentile(latencies, 0.50), Percentile(latencies, 0.95), Percentile(latencies, 0.99)
	s.Max = latencies[len(latencies)-1]
	if elapsed := last.Sub(first).Seconds(); elapsed > 0 {
		s.Rate = float64(len(results)) / elapsed
	}
	return s
}

// Percentile uses the nearest-rank method on sorted values: p95 is the value
// that 95% of requests were at or below.
func Percentile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(q * float64(len(sorted))))
	return sorted[max(rank, 1)-1]
}
