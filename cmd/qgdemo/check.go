package main

import (
	"fmt"
	"net/http"
	"time"

	"quotagate/internal/loadtest"
)

// drain is how long requests already in flight need to finish: the example
// API's QuotaGate client gives up after 2s (QG-07), plus a margin.
const drain = 3 * time.Second

// clockMargin covers clock differences between this program, PostgreSQL and
// Redis when a time decides which minute or which side of the outage
// something belongs to.
const clockMargin = time.Second

type customer struct {
	Name       string `json:"name"`
	DailyLimit int64  `json:"daily_limit"`
	RatePerMin int64  `json:"rate_limit_per_minute"`
}

// The basic customer meets both limits within the demo; the pro customer's
// limits are larger than its whole load, so it must never be refused.
var demoCustomers = []customer{
	{Name: "basic", DailyLimit: 100, RatePerMin: 60},
	{Name: "pro", DailyLimit: 100_000, RatePerMin: 1200},
}

// request is one POST /jobs call and the QuotaGate copy behind its example API.
type request struct {
	Copy int
	loadtest.Result
}

// timeline holds the moments of the outage, all from this program's clock.
type timeline struct {
	LoadStart   time.Time `json:"load_start"`
	StopIssued  time.Time `json:"stop_issued"` // docker compose stop started
	Stopped     time.Time `json:"stopped"`     // ... and returned: the dependency is down
	Unready     time.Time `json:"unready"`     // both copies answer /health/ready with 503
	QuietFrom   time.Time `json:"quiet_from"`  // in-flight requests drained: handler count taken
	QuietTo     time.Time `json:"quiet_to"`    // handler count taken again, just before the restart
	StartIssued time.Time `json:"start_issued"`
	Started     time.Time `json:"started"` // docker compose up --wait returned: dependency healthy
	Ready       time.Time `json:"ready"`   // both copies answer /health/ready with 200 again
	LoadEnd     time.Time `json:"load_end"`
}

// handlerCalls are the example APIs' summed /demo/stats counts.
type handlerCalls struct {
	Before    int64 `json:"before"`
	QuietFrom int64 `json:"quiet_from"`
	QuietTo   int64 `json:"quiet_to"`
	After     int64 `json:"after"`
}

// recorded is what PostgreSQL holds for one customer after the run. Passed
// means the decision got through the Redis rate check: allowed or daily_quota.
type recorded struct {
	Decisions   int64 `json:"decisions"`
	Allowed     int64 `json:"allowed"`
	DailyQuota  int64 `json:"daily_quota"`
	RateLimited int64 `json:"rate_limited"`
	DailyUsed   int64 `json:"daily_used"`
	// Split at the middle of a Redis outage, when no decision can be made:
	// before it the first Redis, after it the restarted one. Without a Redis
	// restart everything is "after".
	PassedBefore  int64 `json:"passed_before"`
	PassedAfter   int64 `json:"passed_after"`
	LimitedBefore int64 `json:"limited_before"`
	LimitedAfter  int64 `json:"limited_after"`
	// PassedStopMinute is how many of PassedBefore fell in the minute window
	// Redis was stopped in, by PostgreSQL decision time.
	PassedStopMinute int64 `json:"passed_in_stop_minute"`
}

// window is one Redis per-minute window of one customer, counted from the
// decision pre-results Redis keeps for 24 hours.
type window struct {
	Start   time.Time `json:"start"`
	Passed  int64     `json:"passed"`
	Limited int64     `json:"limited"`
}

// tally counts what the client saw for one customer.
type tally struct {
	Requests    int64 `json:"requests"`
	Created     int64 `json:"created"`      // 201: handler ran
	DailyQuota  int64 `json:"daily_quota"`  // 429 daily_quota
	RateLimited int64 `json:"rate_limited"` // 429 rate_limited
	Unavailable int64 `json:"unavailable"`  // 503: no decision
	Other       int64 `json:"other"`
	// Uncertain 503s were in flight while the dependency went down or came
	// back, so QuotaGate may have used Redis capacity or committed a
	// PostgreSQL decision before the answer was lost. A 503 during the full
	// outage cannot leave either trace.
	Uncertain int64 `json:"uncertain"`
}

func (t tally) decided() int64 { return t.Created + t.DailyQuota + t.RateLimited }

type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type evidence struct {
	Outage    string // "redis" or "postgres"
	Customers []customer
	Requests  []request
	Timeline  timeline
	Handlers  handlerCalls
	Recorded  []recorded
	Windows   [][]window // per customer, from the Redis that is up at the end
}

func decided(r loadtest.Result) bool {
	return r.Status == http.StatusCreated || r.Status == http.StatusTooManyRequests &&
		(r.Reason == "daily_quota" || r.Reason == "rate_limited")
}

// overlaps reports whether the request was in flight at some moment of [from, to].
func overlaps(r loadtest.Result, from, to time.Time) bool {
	return !r.Sent.After(to) && !r.Done.Before(from)
}

// uncertain reports whether the request was in flight while the dependency
// was going down or coming back up.
func (e evidence) uncertain(r loadtest.Result) bool {
	t := e.Timeline
	return overlaps(r, t.StopIssued, t.Stopped) || overlaps(r, t.StartIssued, t.Ready.Add(drain))
}

func (e evidence) tallies() []tally {
	out := make([]tally, len(e.Customers))
	for _, r := range e.Requests {
		t := &out[r.Customer]
		t.Requests++
		switch {
		case r.Status == http.StatusCreated:
			t.Created++
		case r.Status == http.StatusTooManyRequests && r.Reason == "daily_quota":
			t.DailyQuota++
		case r.Status == http.StatusTooManyRequests && r.Reason == "rate_limited":
			t.RateLimited++
		case r.Status == http.StatusServiceUnavailable:
			t.Unavailable++
			if e.uncertain(r.Result) {
				t.Uncertain++
			}
		default:
			t.Other++
		}
	}
	return out
}

// within reports low <= got <= low+extra.
func within(got, low, extra int64) bool { return low <= got && got <= low+extra }

// reconcile compares what the client saw, the handler counts, PostgreSQL and
// Redis. Every check must pass.
func reconcile(e evidence) []check {
	var checks []check
	add := func(name string, ok bool, format string, args ...any) {
		checks = append(checks, check{Name: name, OK: ok, Detail: fmt.Sprintf(format, args...)})
	}
	t := e.Timeline
	tallies := e.tallies()

	var other, created int64
	for _, x := range tallies {
		other += x.Other
		created += x.Created
	}
	add("every response was 201, a known 429 or 503", other == 0, "%d other responses", other)
	add("handler ran once per 201", e.Handlers.After-e.Handlers.Before == created,
		"handler calls %d, 201 responses %d", e.Handlers.After-e.Handlers.Before, created)

	// Phases: before the outage every request is decided; in the quiet part of
	// the outage every request is refused without running the handler; after
	// recovery every request is decided again. Requests in flight at a
	// transition may end either way.
	var before, beforeBad, quiet, quietBad, after, afterBad, afterCreated int
	var copyBefore, copyAfter [2]int
	for _, r := range e.Requests {
		switch {
		case r.Done.Before(t.StopIssued):
			before++
			if !decided(r.Result) {
				beforeBad++
			}
			if r.Status == http.StatusCreated {
				copyBefore[r.Copy]++
			}
		case !r.Sent.Before(t.QuietFrom) && !r.Done.After(t.QuietTo):
			quiet++
			if r.Status != http.StatusServiceUnavailable {
				quietBad++
			}
		case !r.Sent.Before(t.Ready.Add(drain)):
			after++
			if !decided(r.Result) {
				afterBad++
			}
			if r.Status == http.StatusCreated {
				afterCreated++
				copyAfter[r.Copy]++
			}
		}
	}
	add("before the outage every request got a decision", before > 0 && beforeBad == 0,
		"%d requests, %d without a decision", before, beforeBad)
	add("during the outage every request got 503 and the handler did not run",
		quiet > 0 && quietBad == 0 && e.Handlers.QuietTo == e.Handlers.QuietFrom,
		"%d requests sent %s..%s after the stop, %d not 503; handler calls %d then %d",
		quiet, since(t.LoadStart, t.QuietFrom), since(t.LoadStart, t.QuietTo), quietBad, e.Handlers.QuietFrom, e.Handlers.QuietTo)
	add("after recovery every request got a decision again", after > 0 && afterBad == 0 && afterCreated > 0,
		"%d requests sent from %s, %d without a decision, %d allowed", after, since(t.LoadStart, t.Ready.Add(drain)), afterBad, afterCreated)
	add("both copies allowed requests before and after the outage",
		copyBefore[0] > 0 && copyBefore[1] > 0 && copyAfter[0] > 0 && copyAfter[1] > 0,
		"201s before: copy 1 %d, copy 2 %d; after: copy 1 %d, copy 2 %d", copyBefore[0], copyBefore[1], copyAfter[0], copyAfter[1])

	for i, c := range e.Customers {
		x, db := tallies[i], e.Recorded[i]
		name := c.Name + " customer"
		add(name+": one PostgreSQL record per decided request", within(db.Decisions, x.decided(), x.Uncertain),
			"records %d, decided responses %d, uncertain 503s %d", db.Decisions, x.decided(), x.Uncertain)
		add(name+": PostgreSQL records match responses",
			within(db.Allowed, x.Created, x.Uncertain) && within(db.DailyQuota, x.DailyQuota, x.Uncertain) && within(db.RateLimited, x.RateLimited, x.Uncertain),
			"allowed/daily/rate records %d/%d/%d, responses %d/%d/%d", db.Allowed, db.DailyQuota, db.RateLimited, x.Created, x.DailyQuota, x.RateLimited)
		add(name+": daily usage equals allowed records", db.DailyUsed == db.Allowed, "daily usage %d, allowed %d", db.DailyUsed, db.Allowed)
		add(name+": daily limit kept", db.Allowed <= c.DailyLimit && (db.DailyQuota == 0 || db.Allowed == c.DailyLimit),
			"allowed %d of %d; daily_quota records %d", db.Allowed, c.DailyLimit, db.DailyQuota)
		if c.RatePerMin >= x.Requests && c.DailyLimit >= x.Requests {
			add(name+": limits above its whole load, never refused", x.DailyQuota+x.RateLimited+db.DailyQuota+db.RateLimited == 0,
				"%d requests; 429 responses %d, refusal records %d", x.Requests, x.DailyQuota+x.RateLimited, db.DailyQuota+db.RateLimited)
		}

		var largest, passed, limited int64
		for _, w := range e.Windows[i] {
			largest = max(largest, w.Passed)
			passed += w.Passed
			limited += w.Limited
		}
		add(name+": every Redis window within the per-minute limit", len(e.Windows[i]) > 0 && largest <= c.RatePerMin,
			"%d windows, most passed in one window %d, limit %d", len(e.Windows[i]), largest, c.RatePerMin)
		span := "whole run"
		if e.Outage == "redis" {
			span = "after the Redis restart"
		}
		add(name+": Redis windows match PostgreSQL ("+span+")",
			within(passed, db.PassedAfter, x.Uncertain) && within(limited, db.LimitedAfter, x.Uncertain),
			"Redis passed/limited %d/%d, PostgreSQL %d/%d, uncertain 503s %d", passed, limited, db.PassedAfter, db.LimitedAfter, x.Uncertain)
		if e.Outage == "redis" {
			// The first Redis lost its windows on restart (no persistence). It
			// started empty with the project, so it allowed at most the limit in
			// each minute it was up during the load.
			minutes := minute(t.Stopped.Add(clockMargin)) - minute(t.LoadStart.Add(-clockMargin)) + 1
			add(name+": before the Redis restart, at most the limit per minute",
				db.PassedBefore <= c.RatePerMin*minutes,
				"passed %d in %d minutes, at most %d", db.PassedBefore, minutes, c.RatePerMin*minutes)
		}
	}
	return checks
}

func minute(t time.Time) int64 { return t.UnixMilli() / 60_000 }

// since formats t as seconds after start, like "+31.2s".
func since(start, t time.Time) string { return fmt.Sprintf("+%.1fs", t.Sub(start).Seconds()) }
