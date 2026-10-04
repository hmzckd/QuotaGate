package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"quotagate/internal/store"
)

type lostResponse struct{ header http.Header }

func (w *lostResponse) Header() http.Header       { return w.header }
func (w *lostResponse) WriteHeader(int)           {}
func (w *lostResponse) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestDecisionHTTPWithPostgres(t *testing.T) {
	dsn := os.Getenv("QG_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set QG_TEST_DATABASE_URL to run the PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	customerStore := store.New(pool)
	adminToken := strings.Repeat("b", 40)
	handler := NewAppHandler(pool, customerStore, adminToken)
	call := func(method, path, token, body string, want int) map[string]any {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: got %d, want %d; body=%s", method, path, w.Code, want, w.Body.String())
		}
		var data map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		return data
	}
	create := func(name string, limit int) (string, string) {
		t.Helper()
		created := call("POST", "/v1/admin/customers", adminToken, `{"name":"`+name+`"}`, http.StatusCreated)
		id, key := created["id"].(string), created["api_key"].(string)
		t.Cleanup(func() {
			cleanupCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
			defer done()
			if _, err := pool.Exec(cleanupCtx, `DELETE FROM quotagate.customers WHERE id = $1`, id); err != nil {
				t.Errorf("clean up customer: %v", err)
			}
		})
		call("PUT", "/v1/admin/customers/"+id+"/policies/job.create", adminToken,
			`{"daily_limit":`+stringLimit(limit)+`,"rate_limit_per_minute":100}`, http.StatusOK)
		return id, key
	}
	_, aKey := create("Decision A", 1)
	_, bKey := create("Decision B", 3)

	const lostID = "lost_response_0000001"
	lost := httptest.NewRequest("POST", "/v1/decisions", strings.NewReader(`{"decision_id":"`+lostID+`","operation":"job.create"}`))
	lost.Header.Set("Authorization", "Bearer "+aKey)
	handler.ServeHTTP(&lostResponse{header: make(http.Header)}, lost)
	replayed := call("POST", "/v1/decisions", aKey, `{"decision_id":"`+lostID+`","operation":"job.create"}`, http.StatusOK)
	if replayed["allowed"] != true || replayed["daily_used"] != float64(1) || replayed["reason"] != "allowed" {
		t.Fatalf("committed decision was not found after response loss: %v", replayed)
	}
	again := call("POST", "/v1/decisions", aKey, `{"decision_id":"`+lostID+`","operation":"job.create"}`, http.StatusOK)
	if !reflect.DeepEqual(replayed, again) {
		t.Fatalf("replay changed result: %v != %v", replayed, again)
	}
	denied := call("POST", "/v1/decisions", aKey, `{"decision_id":"another_decision_00001","operation":"job.create"}`, http.StatusOK)
	if denied["allowed"] != false || denied["reason"] != "daily_quota" || denied["daily_used"] != float64(1) {
		t.Fatalf("quota denial: %v", denied)
	}
	call("POST", "/v1/decisions", aKey, `{"decision_id":"`+lostID+`","operation":"other.operation"}`, http.StatusConflict)
	call("POST", "/v1/decisions", aKey, `{"decision_id":"unknown_policy_00001","operation":"other.operation"}`, http.StatusForbidden)
	call("POST", "/v1/decisions", "wrong", `{"decision_id":"unknown_policy_00001","operation":"job.create"}`, http.StatusUnauthorized)
	for _, id := range []string{"b_decision_00000001", "b_decision_00000002"} {
		result := call("POST", "/v1/decisions", bKey, `{"decision_id":"`+id+`","operation":"job.create"}`, http.StatusOK)
		if result["allowed"] != true {
			t.Fatalf("B should have remaining rights: %v", result)
		}
	}
	aUsage := call("GET", "/v1/usage?operation=job.create", aKey, "", http.StatusOK)
	bUsage := call("GET", "/v1/usage?operation=job.create", bKey, "", http.StatusOK)
	if aUsage["daily_used"] != float64(1) || aUsage["daily_limit"] != float64(1) ||
		bUsage["daily_used"] != float64(2) || bUsage["daily_limit"] != float64(3) {
		t.Fatalf("usage crossed customer boundary: A=%v B=%v", aUsage, bUsage)
	}
	call("GET", "/v1/usage?operation=job.create&customer_id=another", aKey, "", http.StatusBadRequest)
	call("GET", "/v1/usage?operation=other.operation", aKey, "", http.StatusForbidden)
}

func stringLimit(limit int) string {
	return strconv.Itoa(limit)
}
