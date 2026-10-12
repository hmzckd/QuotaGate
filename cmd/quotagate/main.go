package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"quotagate/internal/config"
	"quotagate/internal/httpapi"
	"quotagate/internal/migrate"
	"quotagate/internal/ratelimit"
	"quotagate/internal/store"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		// Raw startup errors can contain a DSN. Log only the error's type.
		slog.Error("quotagate stopped", "error_type", fmt.Sprintf("%T", err))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	mode := "serve"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch mode {
	case "migrate":
		conn, err := pgx.Connect(ctx, cfg.DatabaseURL)
		if err != nil {
			return fmt.Errorf("connect database: %w", err)
		}
		defer func() { _ = conn.Close(context.Background()) }()
		if err := migrate.Run(ctx, conn, cfg.MigrationsDir); err != nil {
			return err
		}
		slog.Info("migrations complete")
		return nil
	case "serve", "serve-rate-demo", "serve-combined":
		if len(cfg.AdminToken) < 32 {
			return errors.New("ADMIN_TOKEN must be at least 32 characters")
		}
		pool, err := store.NewPool(ctx, cfg.DatabaseURL)
		if err != nil {
			return fmt.Errorf("database pool: %w", err)
		}
		defer pool.Close()
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = pool.Ping(pingCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("database startup ping: %w", err)
		}
		handler := httpapi.NewAppHandler(pool, store.New(pool), cfg.AdminToken)
		if mode == "serve-rate-demo" || mode == "serve-combined" {
			rate, err := ratelimit.New(cfg.RedisURL)
			if err != nil {
				return err
			}
			defer func() { _ = rate.Close() }()
			redisCtx, redisCancel := context.WithTimeout(ctx, 3*time.Second)
			err = rate.Ping(redisCtx)
			redisCancel()
			if err != nil {
				return err
			}
			if mode == "serve-combined" {
				handler = httpapi.NewCombinedAppHandler(pool, store.New(pool), cfg.AdminToken, rate)
			} else {
				handler = httpapi.NewRateAppHandler(pool, store.New(pool), cfg.AdminToken, rate)
			}
		}
		listener, err := net.Listen("tcp", cfg.HTTPAddr)
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		// Deferred pool and Redis closes run after serve returns, so in-flight
		// requests finish before their connections are closed.
		return serve(ctx, newServer(handler), listener)
	case "prune-decisions":
		return pruneDecisions(ctx, cfg.DatabaseURL, os.Args[2:])
	default:
		return fmt.Errorf("unknown command %q (want serve, serve-rate-demo, serve-combined, migrate or prune-decisions)", mode)
	}
}

// shutdownTimeout is longer than the 4s request budget, so every in-flight
// request can finish before the server stops waiting.
const shutdownTimeout = 10 * time.Second

func newServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		// ReadTimeout also bounds a body sent slowly after the headers.
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
		IdleTimeout:  30 * time.Second,
	}
}

// serve runs until ctx ends, then stops accepting connections and waits for
// in-flight requests to finish.
func serve(ctx context.Context, server *http.Server, listener net.Listener) error {
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	slog.Info("HTTP server listening", "addr", listener.Addr().String())
	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	slog.Info("shutdown started")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		return fmt.Errorf("HTTP shutdown: %w", err)
	}
	slog.Info("shutdown complete")
	return nil
}

// pruneDecisions is a manual maintenance command. Without -apply it only counts.
func pruneDecisions(ctx context.Context, databaseURL string, args []string) error {
	flags := flag.NewFlagSet("prune-decisions", flag.ContinueOnError)
	olderThan := flags.Duration("older-than", 72*time.Hour, "delete decisions older than this; at least 48h")
	apply := flags.Bool("apply", false, "delete the matched decisions; without it the command only counts them")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		// Parsing stops at the first stray word, so "prune-decisions now -apply"
		// would silently ignore -apply. Refuse instead of guessing.
		flags.Usage()
		return errors.New("unexpected argument")
	}
	if *olderThan < store.MinDecisionRetention {
		// main logs only error types, so say what is wrong here.
		slog.Error("retention too short", "older_than", olderThan.String(), "minimum", store.MinDecisionRetention.String())
		return errors.New("retention below minimum")
	}
	// A plain pool: maintenance statements are not bound by the service's 5s statement_timeout.
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("database pool: %w", err)
	}
	defer pool.Close()
	result, err := store.New(pool).PruneDecisions(ctx, *olderThan, *apply)
	if err != nil {
		return err
	}
	slog.Info("decision retention", "apply", *apply, "older_than", olderThan.String(),
		"cutoff", result.Cutoff.UTC().Format(time.RFC3339), "matched", result.Matched, "deleted", result.Deleted)
	return nil
}
