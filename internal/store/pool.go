package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool opens the service pool. Callers bound each query with a context
// deadline (at most 3s, inside a 4s request budget). The server-side limits
// sit just above that: if a caller gives up, PostgreSQL ends its side too
// instead of holding a lock or a transaction open.
func NewPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("database config: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MinConns = 0
	// Without this, a server that accepts TCP but never answers would block a
	// connect that has no context deadline (for example a pool health check).
	cfg.ConnConfig.ConnectTimeout = 2 * time.Second
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "5s"
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "10s"
	return pgxpool.NewWithConfig(ctx, cfg)
}
