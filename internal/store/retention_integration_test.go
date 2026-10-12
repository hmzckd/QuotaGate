package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestPruneDecisionsKeepsYoungerThan48Hours(t *testing.T) {
	s, pool := testStore(t)
	customer := testCustomer(t, s, pool, 100)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Other tests' leftovers must not change the counts below.
	var before int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM quotagate.decisions WHERE created_at < clock_timestamp() - interval '48 hours'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != 0 {
		t.Skipf("test database already has %d decisions older than 48h", before)
	}

	ages := map[string]time.Duration{
		"age_one_hour_000001": time.Hour,
		"age_47_hours_000001": 47 * time.Hour,
		"age_49_hours_000001": 49 * time.Hour,
		"age_100_hours_00001": 100 * time.Hour,
	}
	for id, age := range ages {
		if _, err := s.Decide(ctx, customer.ID, id, "job.create"); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE quotagate.decisions SET created_at = clock_timestamp() - make_interval(secs => $3)
			WHERE customer_id = $1 AND decision_id = $2`, customer.ID, id, age.Seconds()); err != nil {
			t.Fatal(err)
		}
	}
	remaining := func() []string {
		rows, err := pool.Query(ctx, `SELECT decision_id FROM quotagate.decisions WHERE customer_id = $1 ORDER BY created_at DESC`, customer.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		return ids
	}

	if _, err := s.PruneDecisions(ctx, 47*time.Hour, true); err == nil {
		t.Fatal("a retention below 48h was accepted")
	}
	if got := len(remaining()); got != 4 {
		t.Fatalf("rejected prune deleted rows: %d left", got)
	}

	dry, err := s.PruneDecisions(ctx, 48*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Matched != 2 || dry.Deleted != 0 || len(remaining()) != 4 {
		t.Fatalf("dry run = %+v, rows left %d", dry, len(remaining()))
	}

	applied, err := s.PruneDecisions(ctx, 48*time.Hour, true)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Matched != dry.Matched || applied.Deleted != dry.Matched {
		t.Fatalf("apply = %+v, dry run matched %d", applied, dry.Matched)
	}
	if got := fmt.Sprint(remaining()); got != "[age_one_hour_000001 age_47_hours_000001]" {
		t.Fatalf("remaining = %s", got)
	}

	// A replay of a kept decision still returns the stored result.
	if _, err := s.Decide(ctx, customer.ID, "age_47_hours_000001", "job.create"); err != nil {
		t.Fatal(err)
	}
	again, err := s.PruneDecisions(ctx, 48*time.Hour, false)
	if err != nil || again.Matched != 0 {
		t.Fatalf("second dry run = %+v, %v", again, err)
	}
}
