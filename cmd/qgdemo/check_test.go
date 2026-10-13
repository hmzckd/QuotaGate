package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"quotagate/internal/loadtest"
)

var t0 = time.Date(2026, 10, 12, 12, 0, 10, 0, time.UTC)

func at(seconds float64) time.Time { return t0.Add(time.Duration(seconds * float64(time.Second))) }

// sent is a request sent at the given second that took 10ms.
func sent(seconds float64, copy, customer, status int, reason string) request {
	return request{Copy: copy, Result: loadtest.Result{Customer: customer, Status: status, Reason: reason,
		Sent: at(seconds), Done: at(seconds + 0.01)}}
}

// fixture is a small consistent Redis-outage run. The basic customer may pass
// 2 per minute and 3 per day; pro is never limited.
func fixture() evidence {
	const basic, pro = 0, 1
	return evidence{
		Outage:    "redis",
		Customers: []customer{{"basic", 3, 2}, {"pro", 1000, 1000}},
		Timeline: timeline{LoadStart: at(0), StopIssued: at(30), Stopped: at(30.5), Unready: at(31),
			QuietFrom: at(34), QuietTo: at(55), StartIssued: at(55), Started: at(56), Ready: at(56.5), LoadEnd: at(90)},
		Requests: []request{
			sent(1, 0, basic, 201, ""), sent(2, 1, basic, 201, ""), sent(3, 0, basic, 429, "rate_limited"),
			sent(1, 1, pro, 201, ""), sent(2, 0, pro, 201, ""),
			sent(40, 0, basic, 503, ""), sent(41, 1, pro, 503, ""),
			sent(61, 1, basic, 201, ""), sent(62, 0, basic, 429, "daily_quota"),
			sent(61, 0, pro, 201, ""), sent(62, 1, pro, 201, ""),
		},
		Handlers: handlerCalls{Before: 0, QuietFrom: 4, QuietTo: 4, After: 7},
		Recorded: []recorded{
			{Decisions: 5, Allowed: 3, DailyQuota: 1, RateLimited: 1, DailyUsed: 3, PassedBefore: 2, LimitedBefore: 1, PassedAfter: 2},
			{Decisions: 4, Allowed: 4, DailyUsed: 4, PassedBefore: 2, PassedAfter: 2},
		},
		Windows: [][]window{{{Start: at(50), Passed: 2}}, {{Start: at(50), Passed: 2}}},
	}
}

func failed(checks []check) []string {
	var names []string
	for _, c := range checks {
		if !c.OK {
			names = append(names, c.Name)
		}
	}
	return names
}

func TestConsistentRunPasses(t *testing.T) {
	checks := reconcile(fixture())
	if f := failed(checks); len(f) > 0 {
		t.Fatalf("failed checks: %v", f)
	}
	if len(checks) != 21 {
		t.Fatalf("%d checks, want 21", len(checks))
	}
}

func TestReconcileCatchesEachFault(t *testing.T) {
	cases := []struct {
		name   string
		change func(*evidence)
		want   string
	}{
		{"handler ran during the outage", func(e *evidence) { e.Handlers.QuietTo++; e.Handlers.After++ }, "during the outage"},
		{"a 201 during the outage", func(e *evidence) { e.Requests[5].Status = 201; e.Handlers.After++ }, "during the outage"},
		{"a 503 after recovery", func(e *evidence) { e.Requests[9].Status = 503; e.Handlers.After-- }, "after recovery"},
		{"a 503 before the outage", func(e *evidence) { e.Requests[3].Status = 503; e.Handlers.After-- }, "before the outage"},
		{"an unknown status", func(e *evidence) { e.Requests[5].Status = 500 }, "every response"},
		{"handler ran without a 201", func(e *evidence) { e.Handlers.After++ }, "handler ran once"},
		{"one copy never allowed after recovery", func(e *evidence) { e.Requests[7].Copy = 0; e.Requests[10].Copy = 0 }, "both copies"},
		{"a missing PostgreSQL record", func(e *evidence) { e.Recorded[1].Decisions-- }, "pro customer: one PostgreSQL record"},
		{"a record with the wrong reason", func(e *evidence) { e.Recorded[0].DailyQuota--; e.Recorded[0].RateLimited++ }, "basic customer: PostgreSQL records match"},
		{"usage drift", func(e *evidence) { e.Recorded[0].DailyUsed++ }, "basic customer: daily usage"},
		{"daily_quota before the limit", func(e *evidence) { e.Customers[0].DailyLimit = 4 }, "basic customer: daily limit kept"},
		{"pro refused", func(e *evidence) { e.Requests[10].Status, e.Requests[10].Reason = 429, "rate_limited" }, "pro customer: limits above"},
		{"a Redis window over the limit", func(e *evidence) { e.Windows[0][0].Passed = 3 }, "basic customer: every Redis window"},
		{"Redis and PostgreSQL disagree", func(e *evidence) { e.Windows[1][0].Passed = 1 }, "pro customer: Redis windows match"},
		{"too many passed before the restart", func(e *evidence) { e.Recorded[0].PassedBefore = 3 }, "basic customer: before the Redis restart"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := fixture()
			e.Customers = slices.Clone(e.Customers)
			c.change(&e)
			f := failed(reconcile(e))
			if !slices.ContainsFunc(f, func(name string) bool { return strings.Contains(name, c.want) }) {
				t.Fatalf("no failed check contains %q; failed: %v", c.want, f)
			}
		})
	}
}

// postgresFixture is the same run with a PostgreSQL outage instead: Redis
// keeps every window, and nothing is split at a Redis restart.
func postgresFixture() evidence {
	e := fixture()
	e.Outage = "postgres"
	for i := range e.Recorded {
		x := &e.Recorded[i]
		x.PassedAfter, x.LimitedAfter, x.PassedBefore, x.LimitedBefore = x.PassedBefore+x.PassedAfter, x.LimitedBefore+x.LimitedAfter, 0, 0
	}
	e.Windows = [][]window{
		{{Start: at(0), Passed: 2, Limited: 1}, {Start: at(60), Passed: 2}},
		{{Start: at(0), Passed: 2}, {Start: at(60), Passed: 2}},
	}
	return e
}

// A 503 in flight while the dependency went down may still have left a
// decision in PostgreSQL and Redis, so it widens the accepted range by one.
// A 503 sent during the full outage cannot, so it does not.
func TestUncertainOnlyAtTransitions(t *testing.T) {
	e := postgresFixture()
	if f := failed(reconcile(e)); len(f) > 0 {
		t.Fatalf("failed checks: %v", f)
	}
	// The basic customer's 503 left a rate_limited decision behind.
	e.Recorded[0].Decisions++
	e.Recorded[0].RateLimited++
	e.Recorded[0].LimitedAfter++
	e.Windows[0][0].Limited++
	if f := failed(reconcile(e)); len(f) == 0 {
		t.Fatal("a record for a 503 sent during the full outage was accepted")
	}
	// The same 503, in flight while PostgreSQL went down.
	e.Requests[5].Sent, e.Requests[5].Done = at(29.9), at(30.2)
	if got := e.tallies()[0].Uncertain; got != 1 {
		t.Fatalf("uncertain = %d, want 1", got)
	}
	if f := failed(reconcile(e)); len(f) > 0 {
		t.Fatalf("failed checks: %v", f)
	}
}

func TestBucketsAndCopies(t *testing.T) {
	e := fixture()
	b := buckets(e)
	if len(b) != 13 || b[0].Requests != 5 || b[8].Unavailable != 2 || b[12].Created != 3 || b[12].Refused != 1 {
		t.Fatalf("buckets = %+v", b)
	}
	rows := copyRows(e.Requests)
	if rows[0].Created != 3 || rows[0].Requests != 6 || rows[1].Created != 4 || rows[1].Requests != 5 {
		t.Fatalf("copies = %+v", rows)
	}
}
