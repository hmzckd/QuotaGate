package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRequestBudgetBoundsTheWholeRequest(t *testing.T) {
	var remaining time.Duration
	handler := withRequestBudget(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline, ok := r.Context().Deadline()
		if !ok {
			t.Fatal("request has no deadline")
		}
		remaining = time.Until(deadline)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/me", nil))
	if remaining <= 0 || remaining > requestBudget {
		t.Fatalf("remaining budget %s, want (0, %s]", remaining, requestBudget)
	}
}
