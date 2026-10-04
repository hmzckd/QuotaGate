package client

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
)

// Middleware creates a fresh decision ID for each inbound request. Separate
// inbound requests may both run the handler, even when their bodies are equal.
func (c *Client) Middleware(operation string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		key, ok := bearerKey(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		var randomID [16]byte
		if _, err := rand.Read(randomID[:]); err != nil {
			writeError(w, http.StatusServiceUnavailable, "decision unavailable")
			return
		}
		decision, err := c.Decide(r.Context(), key, hex.EncodeToString(randomID[:]), operation)
		if err != nil {
			var upstream *HTTPError
			if errors.As(err, &upstream) {
				switch upstream.StatusCode {
				case http.StatusUnauthorized:
					w.Header().Set("WWW-Authenticate", "Bearer")
					writeError(w, http.StatusUnauthorized, "unauthorized")
					return
				case http.StatusForbidden:
					writeError(w, http.StatusForbidden, "operation policy not found")
					return
				}
			}
			writeError(w, http.StatusServiceUnavailable, "decision unavailable")
			return
		}
		if !decision.Allowed {
			writeError(w, http.StatusTooManyRequests, decision.Reason)
			return
		}
		if r.Context().Err() != nil {
			writeError(w, http.StatusServiceUnavailable, "request canceled before handler")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{message})
}
