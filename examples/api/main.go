package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"quotagate/client"
)

func main() {
	if err := run(); err != nil {
		slog.Error("example API stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8081"
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return errors.New("HTTP_ADDR must be host:port")
	}
	var guard func(http.Handler) http.Handler
	switch os.Getenv("QUOTAGATE_MIDDLEWARE") {
	case "", "on":
		decisions, err := client.New(os.Getenv("QUOTAGATE_URL"))
		if err != nil {
			return err
		}
		defer decisions.Close()
		guard = quotaGuard(decisions)
	case "off":
		// Load-test baseline only (QG-11): the same handler without a decision call.
		slog.Warn("QuotaGate middleware disabled; every /jobs request runs the handler")
		guard = func(next http.Handler) http.Handler { return next }
	default:
		return errors.New(`QUOTAGATE_MIDDLEWARE must be "on" or "off"`)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{
		Addr:              addr,
		Handler:           newHandler(guard),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()
	slog.Info("example API listening", "addr", addr)
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
			return fmt.Errorf("example API shutdown: %w", err)
		}
		return nil
	}
}

func quotaGuard(decisions *client.Client) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler { return decisions.Middleware("job.create", next) }
}

// newHandler wraps POST /jobs with guard: the QuotaGate middleware, or nothing
// for the load-test baseline.
func newHandler(guard func(http.Handler) http.Handler) http.Handler {
	var calls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "live"})
	})
	// This aggregate process-local counter is only evidence for the local demo.
	mux.HandleFunc("GET /demo/stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]int64{"handler_calls": calls.Load()})
	})
	mux.Handle("POST /jobs", guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := calls.Add(1)
		writeJSON(w, http.StatusCreated, struct {
			JobID        string `json:"job_id"`
			HandlerCalls int64  `json:"handler_calls"`
		}{"demo-" + strconv.FormatInt(count, 10), count})
	})))
	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
