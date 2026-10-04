package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"quotagate/client"
)

func TestExampleJobsAndHandlerEvidence(t *testing.T) {
	var decisions atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if decisions.Add(1) <= 2 {
			_, _ = io.WriteString(w, `{"allowed":true,"reason":"allowed","utc_day":"2026-10-03","daily_used":1,"daily_limit":2,"policy_version":1}`)
		} else {
			_, _ = io.WriteString(w, `{"allowed":false,"reason":"daily_quota","utc_day":"2026-10-03","daily_used":2,"daily_limit":2,"policy_version":1}`)
		}
	}))
	t.Cleanup(upstream.Close)
	c, err := client.New(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	handler := newHandler(c)
	call := func(method, path string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", "Bearer qg_"+strings.Repeat("a", 64))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: status = %d, want %d", method, path, w.Code, want)
		}
		return w
	}
	first := call("POST", "/jobs", 201)
	second := call("POST", "/jobs", 201)
	if !strings.Contains(first.Body.String(), `"job_id":"demo-1"`) || !strings.Contains(second.Body.String(), `"job_id":"demo-2"`) {
		t.Fatal("allowed requests did not each create one demo job")
	}
	call("POST", "/jobs", 429)
	upstream.Close()
	call("POST", "/jobs", 503)
	stats := call("GET", "/demo/stats", 200)
	if strings.TrimSpace(stats.Body.String()) != `{"handler_calls":2}` {
		t.Fatalf("denial/outage changed the handler count: %s", stats.Body.String())
	}
	call("GET", "/jobs", 405)
	call("GET", "/health/live", 200)
}
