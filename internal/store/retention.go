package store

import (
	"context"
	"fmt"
	"time"
)

// MinDecisionRetention protects replay: a decision ID may be repeated for 24
// hours (the Redis pre-result TTL), and a stored decision is kept at least
// twice that. Deleting a decision earlier would let a late replay spend quota again.
const MinDecisionRetention = 48 * time.Hour

const pruneBatch = 1000

type PruneResult struct {
	Cutoff  time.Time
	Matched int64
	Deleted int64
}

// PruneDecisions counts decisions created before now-olderThan and, with
// apply, deletes them in short batches so no lock is held for long. The
// database clock sets the cutoff, the same clock that sets created_at.
func (s *Store) PruneDecisions(ctx context.Context, olderThan time.Duration, apply bool) (PruneResult, error) {
	if olderThan < MinDecisionRetention {
		return PruneResult{}, fmt.Errorf("retention %s is below the %s minimum", olderThan, MinDecisionRetention)
	}
	var result PruneResult
	err := s.db.QueryRow(ctx, `
		SELECT c.cutoff, (SELECT count(*) FROM quotagate.decisions WHERE created_at < c.cutoff)
		FROM (SELECT clock_timestamp() - make_interval(secs => $1) AS cutoff) AS c`,
		olderThan.Seconds(),
	).Scan(&result.Cutoff, &result.Matched)
	if err != nil {
		return PruneResult{}, fmt.Errorf("count old decisions: %w", err)
	}
	if !apply {
		return result, nil
	}
	for {
		tag, err := s.db.Exec(ctx, `
			DELETE FROM quotagate.decisions
			WHERE (customer_id, decision_id) IN (
				SELECT customer_id, decision_id FROM quotagate.decisions
				WHERE created_at < $1 ORDER BY created_at LIMIT $2)`,
			result.Cutoff, pruneBatch,
		)
		if err != nil {
			return result, fmt.Errorf("delete old decisions: %w", err)
		}
		result.Deleted += tag.RowsAffected()
		if tag.RowsAffected() < pruneBatch {
			return result, nil
		}
	}
}
