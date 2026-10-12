package httpapi

import (
	"context"
	"net/http"
	"time"
)

// requestBudget bounds a whole request. Each dependency call still has its own
// 3s limit (requestContext), but two calls in a row cannot exceed this total.
// It stays below the server's 5s WriteTimeout so a late 503 is still delivered.
const requestBudget = 4 * time.Second

func withRequestBudget(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), requestBudget)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
