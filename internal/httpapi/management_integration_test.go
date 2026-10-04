package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"quotagate/internal/store"
)

func TestManagementWithPostgres(t *testing.T) {
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
	adminToken := strings.Repeat("a", 40)
	handler := NewAppHandler(pool, store.New(pool), adminToken)
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
		if want == http.StatusNoContent {
			return nil
		}
		var data map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		return data
	}

	call("POST", "/v1/admin/customers", "wrong-token", `{"name":"Should fail"}`, http.StatusUnauthorized)
	a := call("POST", "/v1/admin/customers", adminToken, `{"name":"Customer A"}`, http.StatusCreated)
	b := call("POST", "/v1/admin/customers", adminToken, `{"name":"Customer B"}`, http.StatusCreated)
	aID, aKey := a["id"].(string), a["api_key"].(string)
	bID, bKey := b["id"].(string), b["api_key"].(string)
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM quotagate.customers WHERE id = ANY($1)`, []string{aID, bID})
	})
	if aKey == bKey || len(aKey) != 67 || len(bKey) != 67 {
		t.Fatal("customer keys must be distinct 256-bit keys")
	}
	var storedHash []byte
	if err := pool.QueryRow(ctx, `SELECT api_key_hash FROM quotagate.customers WHERE id = $1`, aID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256([]byte(aKey))
	if string(storedHash) != string(wantHash[:]) {
		t.Fatal("customer key was not stored as its SHA-256 hash")
	}

	aPolicy := fmt.Sprintf("/v1/admin/customers/%s/policies/job.create", aID)
	bPolicy := fmt.Sprintf("/v1/admin/customers/%s/policies/job.create", bID)
	call("PUT", aPolicy, adminToken, `{"daily_limit":3,"rate_limit_per_minute":4}`, http.StatusOK)
	call("PUT", bPolicy, adminToken, `{"daily_limit":7,"rate_limit_per_minute":8}`, http.StatusOK)
	updated := call("PUT", aPolicy, adminToken, `{"daily_limit":5,"rate_limit_per_minute":4}`, http.StatusOK)
	if updated["version"] != float64(2) {
		t.Fatalf("policy version got %v, want 2", updated["version"])
	}
	for _, tc := range []struct {
		id, key string
		limit   float64
	}{
		{aID, aKey, 5},
		{bID, bKey, 7},
	} {
		me := call("GET", "/v1/me", tc.key, "", http.StatusOK)
		if me["id"] != tc.id {
			t.Fatalf("key resolved to wrong customer: %v", me)
		}
		policy := call("GET", "/v1/policies/job.create", tc.key, "", http.StatusOK)
		if policy["customer_id"] != tc.id || policy["daily_limit"] != tc.limit {
			t.Fatalf("policy crossed customer boundary: %v", policy)
		}
	}
	call("GET", "/v1/policies/job.create?customer_id="+bID, aKey, "", http.StatusBadRequest)
	call("GET", "/v1/policies/job.create", adminToken, "", http.StatusUnauthorized)

	rotated := call("POST", "/v1/admin/customers/"+aID+"/rotate-key", adminToken, "", http.StatusOK)
	newAKey := rotated["api_key"].(string)
	if newAKey == aKey {
		t.Fatal("rotation reused old key")
	}
	call("GET", "/v1/me", aKey, "", http.StatusUnauthorized)
	call("GET", "/v1/me", newAKey, "", http.StatusOK)
	call("POST", "/v1/admin/customers/"+bID+"/disable", adminToken, "", http.StatusNoContent)
	call("GET", "/v1/me", bKey, "", http.StatusUnauthorized)
	call("PUT", bPolicy, adminToken, `{"daily_limit":9,"rate_limit_per_minute":9}`, http.StatusNotFound)
}
