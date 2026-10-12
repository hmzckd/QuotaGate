package store

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// silentServer accepts TCP connections and never answers, like a hung database.
func silentServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			conn.Close()
		}
	})
	return listener.Addr().String()
}

func TestPoolGivesUpOnSilentServerWithoutCallerDeadline(t *testing.T) {
	pool, err := NewPool(context.Background(), "postgres://qg@"+silentServer(t)+"/quotagate?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	start := time.Now()
	err = pool.Ping(context.Background()) // no deadline from the caller
	if err == nil {
		t.Fatal("ping to a silent server succeeded")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("connect took %s; ConnectTimeout should end it near 2s", elapsed)
	}
}

func TestPoolSetsServerSideTimeouts(t *testing.T) {
	dsn := os.Getenv("QG_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set QG_TEST_DATABASE_URL for PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var statement, idle string
	if err := pool.QueryRow(ctx, `SELECT current_setting('statement_timeout'), current_setting('idle_in_transaction_session_timeout')`).Scan(&statement, &idle); err != nil {
		t.Fatal(err)
	}
	if statement != "5s" || idle != "10s" {
		t.Fatalf("statement_timeout=%s idle_in_transaction_session_timeout=%s", statement, idle)
	}

	// The server-side limit ends a query even when the caller sets no
	// deadline at all, for example code that forgot one.
	start := time.Now()
	_, err = pool.Exec(context.Background(), `SELECT pg_sleep(8)`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("slow query error = %v, want query_canceled (57014)", err)
	}
	if elapsed := time.Since(start); elapsed < 4*time.Second || elapsed > 7*time.Second {
		t.Fatalf("slow query ended after %s, want about 5s", elapsed)
	}
}
