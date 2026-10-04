package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakePinger struct{ err error }

func (p fakePinger) Ping(context.Context) error { return p.err }

func TestHealthSeparatesLivenessFromDatabaseReadiness(t *testing.T) {
	for _, tc := range []struct {
		name        string
		pingErr     error
		readyStatus int
	}{
		{"database up", nil, http.StatusOK},
		{"database down", errors.New("connection refused"), http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := NewHandler(fakePinger{tc.pingErr})
			for path, want := range map[string]int{
				"/health/live":  http.StatusOK,
				"/health/ready": tc.readyStatus,
			} {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
				if response.Code != want {
					t.Fatalf("%s: got %d, want %d", path, response.Code, want)
				}
			}
		})
	}
}
