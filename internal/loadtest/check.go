package loadtest

import (
	"fmt"
	"net/http"
	"time"
)

// Recorded is what PostgreSQL holds for one test customer after the run.
type Recorded struct {
	Decisions   int64 `json:"decisions"`
	Allowed     int64 `json:"allowed"`
	DailyQuota  int64 `json:"daily_quota"`
	RateLimited int64 `json:"rate_limited"`
	DailyUsed   int64 `json:"daily_used"`
}

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// clockMargin covers clock differences between this client and Redis when
// deciding which one-minute rate window a request fell into.
const clockMargin = time.Second

// Reconcile compares what the client saw on the QuotaGate run with what
// PostgreSQL recorded (recorded[i] belongs to customers[i]) and with the
// example API's handler count. Every check must pass: a wrong or missing
// decision fails the run whatever the latency was.
func Reconcile(customers []Customer, results []Result, recorded []Recorded, handlerCalls int64) []Check {
	type seen struct{ sent, allowed, daily, rate, errors int64 }
	client := make([]seen, len(customers))
	for _, r := range results {
		c := &client[r.Customer]
		c.sent++
		switch {
		case r.Status == http.StatusCreated:
			c.allowed++
		case r.Status == http.StatusTooManyRequests && r.Reason == "daily_quota":
			c.daily++
		case r.Status == http.StatusTooManyRequests && r.Reason == "rate_limited":
			c.rate++
		default:
			c.errors++
		}
	}

	var checks []Check
	add := func(name string, ok bool, format string, args ...any) {
		checks = append(checks, Check{Name: name, OK: ok, Detail: fmt.Sprintf(format, args...)})
	}
	var errors, allowed int64
	for _, c := range client {
		errors += c.errors
		allowed += c.allowed
	}
	add("every request got 201 or a known 429", errors == 0, "%d other responses", errors)
	add("handler ran once per 201", handlerCalls == allowed, "handler calls %d, 201 responses %d", handlerCalls, allowed)
	for i, customer := range customers {
		c, db := client[i], recorded[i]
		name := fmt.Sprintf("customer %d (%s)", i+1, customer.Kind)
		add(name+": one decision record per request", db.Decisions == c.sent,
			"records %d, requests %d", db.Decisions, c.sent)
		add(name+": records match responses", db.Allowed == c.allowed && db.DailyQuota == c.daily && db.RateLimited == c.rate,
			"allowed/daily/rate records %d/%d/%d, responses %d/%d/%d", db.Allowed, db.DailyQuota, db.RateLimited, c.allowed, c.daily, c.rate)
		add(name+": daily usage equals allowed", db.DailyUsed == db.Allowed, "daily usage %d, allowed %d", db.DailyUsed, db.Allowed)
		switch customer.Kind {
		case Open:
			add(name+": every request allowed", c.allowed == c.sent, "allowed %d of %d", c.allowed, c.sent)
		case Daily:
			want := min(c.sent, customer.DailyLimit)
			add(name+": exactly the daily limit allowed", c.allowed == want && c.rate == 0,
				"allowed %d, want %d; rate_limited %d", c.allowed, want, c.rate)
		case Rate:
			low, high := rateBounds(results, i, int64(customer.RatePerMin))
			add(name+": per-minute limit kept", low <= c.allowed && c.allowed <= high && c.daily == 0,
				"allowed %d, possible %d..%d; daily_quota %d", c.allowed, low, high, c.daily)
		}
	}
	return checks
}

// rateBounds returns the fewest and most allowed decisions customer c's
// per-minute limit permits, given when its requests were sent and answered.
// Redis counts per clock minute and decides between send and response.
// A request whose whole span sits inside one minute was decided there, so
// that minute must allow at least min(limit, such requests). Every minute any
// request may have touched allows at most limit.
func rateBounds(results []Result, c int, limit int64) (low, high int64) {
	inside := map[int64]int64{}
	touched := map[int64]bool{}
	for _, r := range results {
		if r.Customer != c {
			continue
		}
		from, to := minute(r.Sent.Add(-clockMargin)), minute(r.Done.Add(clockMargin))
		for m := from; m <= to; m++ {
			touched[m] = true
		}
		if from == to {
			inside[from]++
		}
	}
	for _, n := range inside {
		low += min(n, limit)
	}
	return low, int64(len(touched)) * limit
}

func minute(t time.Time) int64 { return t.UnixMilli() / 60_000 }
