package loadtest

import (
	"testing"
	"time"
)

func TestPercentileNearestRank(t *testing.T) {
	var sorted []time.Duration
	for i := 1; i <= 100; i++ {
		sorted = append(sorted, time.Duration(i)*time.Millisecond)
	}
	for q, want := range map[float64]time.Duration{0.50: 50 * time.Millisecond, 0.95: 95 * time.Millisecond, 0.99: 99 * time.Millisecond, 1: 100 * time.Millisecond} {
		if got := Percentile(sorted, q); got != want {
			t.Fatalf("p%.0f = %s, want %s", q*100, got, want)
		}
	}
	if got := Percentile(sorted[:1], 0.95); got != time.Millisecond {
		t.Fatalf("one value: p95 = %s", got)
	}
}

func TestSummarizeCountsErrorsAndReasons(t *testing.T) {
	start := time.Now()
	results := []Result{
		{Status: 201, Latency: 10 * time.Millisecond},
		{Status: 429, Reason: "daily_quota", Latency: 20 * time.Millisecond},
		{Status: 429, Reason: "rate_limited", Latency: 30 * time.Millisecond},
		{Status: 503, Latency: 40 * time.Millisecond},
		{Status: 0, Latency: 50 * time.Millisecond}, // no response
	}
	for i := range results {
		results[i].Sent = start.Add(time.Duration(i) * time.Second)
		results[i].Done = results[i].Sent.Add(results[i].Latency)
	}
	s := Summarize(results)
	if s.Errors != 2 || s.Reasons["daily_quota"] != 1 || s.Reasons["rate_limited"] != 1 || s.Statuses[0] != 1 {
		t.Fatalf("summary = %+v", s)
	}
	if s.P50 != 30*time.Millisecond || s.Max != 50*time.Millisecond {
		t.Fatalf("p50 = %s, max = %s", s.P50, s.Max)
	}
	if s.Rate < 1.2 || s.Rate > 1.3 { // 5 requests over 4.05s
		t.Fatalf("rate = %.2f", s.Rate)
	}
}
