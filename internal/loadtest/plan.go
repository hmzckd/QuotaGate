// Package loadtest drives the QG-11 load comparison: a seeded request plan,
// an open-loop runner, latency summaries and a reconciliation of what the
// client saw against what PostgreSQL recorded.
package loadtest

import "math/rand/v2"

// Kind says which limit a test customer is meant to reach.
type Kind string

const (
	Open  Kind = "open"  // limits far above the load: every decision is allowed
	Daily Kind = "daily" // small daily limit: exactly that many are allowed
	Rate  Kind = "rate"  // small per-minute limit: at most that many per window
)

type Customer struct {
	Kind       Kind  `json:"kind"`
	DailyLimit int64 `json:"daily_limit"`
	RatePerMin int32 `json:"rate_limit_per_minute"`
	// Weight is this customer's share of the plan, out of the sum of weights.
	Weight int `json:"weight"`
}

// DefaultCustomers sends 70% of the load to eight open customers and 15% each
// to one daily-limited and one rate-limited customer. At 50 req/s for 5 min the
// daily customer gets about 2250 requests for 500 allowed, and the rate
// customer about 450 per minute for 120 allowed per window.
func DefaultCustomers() []Customer {
	customers := make([]Customer, 0, 10)
	for range 8 {
		customers = append(customers, Customer{Kind: Open, DailyLimit: 1_000_000_000, RatePerMin: 1_000_000, Weight: 7})
	}
	return append(customers,
		Customer{Kind: Daily, DailyLimit: 500, RatePerMin: 1_000_000, Weight: 12},
		Customer{Kind: Rate, DailyLimit: 1_000_000_000, RatePerMin: 120, Weight: 12},
	)
}

// Plan returns the customer index of each of n requests. The same seed always
// gives the same plan, so the baseline and QuotaGate runs send the same load
// and a later run can repeat it.
func Plan(customers []Customer, n int, seed uint64) []int {
	random := rand.New(rand.NewPCG(seed, seed))
	total := 0
	for _, c := range customers {
		total += c.Weight
	}
	plan := make([]int, n)
	for i := range plan {
		pick := random.IntN(total)
		for c, customer := range customers {
			if pick < customer.Weight {
				plan[i] = c
				break
			}
			pick -= customer.Weight
		}
	}
	return plan
}

// WarmupPlan spreads n requests over the open customers only, so warm-up
// traffic never uses the daily or rate customers' limited capacity.
func WarmupPlan(customers []Customer, n int) []int {
	var open []int
	for c, customer := range customers {
		if customer.Kind == Open {
			open = append(open, c)
		}
	}
	plan := make([]int, n)
	for i := range plan {
		plan[i] = open[i%len(open)]
	}
	return plan
}
