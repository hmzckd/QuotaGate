package loadtest

import (
	"slices"
	"testing"
)

func TestPlanIsRepeatableAndFollowsWeights(t *testing.T) {
	customers := DefaultCustomers()
	a, b := Plan(customers, 100_000, 11), Plan(customers, 100_000, 11)
	if !slices.Equal(a, b) {
		t.Fatal("the same seed gave two different plans")
	}
	if slices.Equal(a, Plan(customers, 100_000, 12)) {
		t.Fatal("a different seed gave the same plan")
	}
	counts := make([]int, len(customers))
	for _, c := range a {
		counts[c]++
	}
	total := 0
	for _, c := range customers {
		total += c.Weight
	}
	for i, c := range customers {
		want := float64(c.Weight) / float64(total)
		if got := float64(counts[i]) / float64(len(a)); got < want-0.01 || got > want+0.01 {
			t.Fatalf("customer %d got %.3f of the plan, want about %.3f", i, got, want)
		}
	}
}

func TestWarmupUsesOnlyOpenCustomers(t *testing.T) {
	customers := DefaultCustomers()
	for _, c := range WarmupPlan(customers, 500) {
		if customers[c].Kind != Open {
			t.Fatalf("warm-up used a %s customer", customers[c].Kind)
		}
	}
}
