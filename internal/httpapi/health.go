package httpapi

import (
	"context"
	"net/http"
	"time"
)

type Pinger interface {
	Ping(context.Context) error
}

func NewHandler(db Pinger) http.Handler {
	o := newObservation()
	return o.wrap(newHealthMux(db, nil, o))
}

func newHealthMux(db Pinger, rate Pinger, o *observation) *http.ServeMux {
	o.dependency.WithLabelValues("postgres").Set(-1)
	if rate != nil {
		o.dependency.WithLabelValues("redis").Set(-1)
	}
	dependencies := observedReadiness{db: db, rate: rate, obs: o}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("live\n"))
	})
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if err := dependencies.Ping(ctx); err != nil {
			http.Error(w, "dependency unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})
	return mux
}
