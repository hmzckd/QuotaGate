package loadtest

import (
	"strings"
	"testing"
	"time"
)

// scenario: one open, one daily (limit 2) and one rate (limit 3 per minute)
// customer, all requests 20s into one clock minute.
func scenario() ([]Customer, []Result, []Recorded, int64) {
	customers := []Customer{
		{Kind: Open, DailyLimit: 100, RatePerMin: 100, Weight: 1},
		{Kind: Daily, DailyLimit: 2, RatePerMin: 100, Weight: 1},
		{Kind: Rate, DailyLimit: 100, RatePerMin: 3, Weight: 1},
	}
	at := time.Now().Truncate(time.Minute).Add(20 * time.Second)
	var results []Result
	add := func(customer, status int, reason string) {
		sent := at.Add(time.Duration(len(results)) * 10 * time.Millisecond)
		results = append(results, Result{Customer: customer, Status: status, Reason: reason, Sent: sent, Done: sent.Add(5 * time.Millisecond)})
	}
	add(0, 201, "")
	add(0, 201, "")
	add(1, 201, "")
	add(1, 201, "")
	add(1, 429, "daily_quota")
	for range 3 {
		add(2, 201, "")
	}
	add(2, 429, "rate_limited")
	add(2, 429, "rate_limited")
	recorded := []Recorded{
		{Decisions: 2, Allowed: 2, DailyUsed: 2},
		{Decisions: 3, Allowed: 2, DailyQuota: 1, DailyUsed: 2},
		{Decisions: 5, Allowed: 3, RateLimited: 2, DailyUsed: 3},
	}
	return customers, results, recorded, 7
}

func failed(checks []Check) []string {
	var names []string
	for _, c := range checks {
		if !c.OK {
			names = append(names, c.Name)
		}
	}
	return names
}

func TestReconcilePassesMatchingRun(t *testing.T) {
	customers, results, recorded, handler := scenario()
	if f := failed(Reconcile(customers, results, recorded, handler)); len(f) > 0 {
		t.Fatalf("failed checks: %v", f)
	}
}

func TestReconcileCatchesWrongOrMissingDecisions(t *testing.T) {
	cases := map[string]struct {
		change func([]Result, []Recorded, *int64)
		want   string
	}{
		"missing record": {func(_ []Result, rec []Recorded, _ *int64) { rec[0].Decisions-- }, "customer 1 (open): one decision record per request"},
		"usage drift":    {func(_ []Result, rec []Recorded, _ *int64) { rec[1].DailyUsed++ }, "customer 2 (daily): daily usage equals allowed"},
		"daily over limit": {func(res []Result, rec []Recorded, h *int64) {
			res[4].Status, res[4].Reason = 201, ""
			rec[1].Allowed, rec[1].DailyQuota, rec[1].DailyUsed = 3, 0, 3
			*h = 8
		}, "customer 2 (daily): exactly the daily limit allowed"},
		"rate over limit": {func(res []Result, rec []Recorded, h *int64) {
			res[8].Status, res[8].Reason = 201, ""
			rec[2].Allowed, rec[2].RateLimited, rec[2].DailyUsed = 4, 1, 4
			*h = 8
		}, "customer 3 (rate): per-minute limit kept"},
		"handler extra": {func(_ []Result, _ []Recorded, h *int64) { *h = 8 }, "handler ran once per 201"},
		"unavailable":   {func(res []Result, _ []Recorded, _ *int64) { res[0].Status = 503 }, "every request got 201 or a known 429"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			customers, results, recorded, handler := scenario()
			tc.change(results, recorded, &handler)
			f := failed(Reconcile(customers, results, recorded, handler))
			if !strings.Contains(strings.Join(f, "\n"), tc.want) {
				t.Fatalf("failed checks %v, want %q among them", f, tc.want)
			}
		})
	}
}

func TestRateBoundsAcrossMinuteBoundary(t *testing.T) {
	m := time.Now().Truncate(time.Minute)
	var results []Result
	for _, offset := range []time.Duration{-30 * time.Second, -20 * time.Second, -10 * time.Second, -200 * time.Millisecond, 10 * time.Second, 20 * time.Second} {
		results = append(results, Result{Customer: 0, Sent: m.Add(offset), Done: m.Add(offset + 50*time.Millisecond)})
	}
	// Three requests surely in the first minute, two surely in the second and
	// one too close to the boundary to place.
	low, high := rateBounds(results, 0, 2)
	if low != 4 || high != 4 {
		t.Fatalf("bounds %d..%d, want 4..4", low, high)
	}
}
