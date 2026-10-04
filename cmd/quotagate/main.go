package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	if err := run(); err != nil {
		slog.Error("quotagate stopped", "error", err)
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
		poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
		if err != nil {
			return fmt.Errorf("database config: %w", err)
		}
		poolCfg.MaxConns = 10
		poolCfg.MinConns = 0
		pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
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
		server := &http.Server{
			Addr:              cfg.HTTPAddr,
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			WriteTimeout:      5 * time.Second,
			IdleTimeout:       30 * time.Second,
		}
		serveErr := make(chan error, 1)
		go func() { serveErr <- server.ListenAndServe() }()
		slog.Info("HTTP server listening", "addr", cfg.HTTPAddr)
		select {
		case err := <-serveErr:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil {
				_ = server.Close()
				return fmt.Errorf("HTTP shutdown: %w", err)
			}
			return nil
		}
	default:
		return fmt.Errorf("unknown command %q (want serve, serve-rate-demo or migrate)", mode)
	}
}
