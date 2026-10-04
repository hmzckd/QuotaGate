package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"quotagate/internal/store"
)

type CustomerStore interface {
	CreateCustomer(context.Context, string) (store.Customer, string, error)
	Authenticate(context.Context, string) (store.Customer, error)
	RotateKey(context.Context, string) (string, error)
	DisableCustomer(context.Context, string) error
	PutPolicy(context.Context, store.Policy) (store.Policy, error)
	GetPolicy(context.Context, string, string) (store.Policy, error)
	Decide(context.Context, string, string, string) (store.Decision, error)
	Usage(context.Context, string, string) (store.Usage, error)
}

var operationName = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)*$`)
var decisionName = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

type management struct {
	store     CustomerStore
	adminHash [32]byte
	rate      RateLimiter
}

func NewAppHandler(db Pinger, customerStore CustomerStore, adminToken string) http.Handler {
	return newAppHandler(db, customerStore, adminToken, nil)
}

func newAppHandler(db Pinger, customerStore CustomerStore, adminToken string, rate RateLimiter) http.Handler {
	mux := newHealthMux(db)
	m := &management{store: customerStore, adminHash: sha256.Sum256([]byte(adminToken)), rate: rate}
	mux.Handle("POST /v1/admin/customers", m.adminOnly(http.HandlerFunc(m.createCustomer)))
	mux.Handle("POST /v1/admin/customers/{customerID}/rotate-key", m.adminOnly(http.HandlerFunc(m.rotateKey)))
	mux.Handle("POST /v1/admin/customers/{customerID}/disable", m.adminOnly(http.HandlerFunc(m.disableCustomer)))
	mux.Handle("PUT /v1/admin/customers/{customerID}/policies/{operation}", m.adminOnly(http.HandlerFunc(m.putPolicy)))
	mux.HandleFunc("GET /v1/me", m.me)
	mux.HandleFunc("GET /v1/policies/{operation}", m.getPolicy)
	mux.HandleFunc("POST /v1/decisions", m.decide)
	mux.HandleFunc("GET /v1/usage", m.usage)
	if rate != nil {
		mux.HandleFunc("POST /demo/rate-checks", m.rateCheck)
	}
	return mux
}

func (m *management) adminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			unauthorized(w)
			return
		}
		providedHash := sha256.Sum256([]byte(token))
		if subtle.ConstantTimeCompare(providedHash[:], m.adminHash[:]) != 1 {
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (m *management) createCustomer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if !validName(name) {
		writeError(w, http.StatusBadRequest, "invalid customer name")
		return
	}
	ctx, cancel := requestContext(r)
	defer cancel()
	customer, key, err := m.store.CreateCustomer(ctx, name)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "customer storage unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, struct {
		store.Customer
		APIKey string `json:"api_key"`
	}{customer, key})
}

func (m *management) rotateKey(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := requestContext(r)
	defer cancel()
	key, err := m.store.RotateKey(ctx, r.PathValue("customerID"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, struct {
		APIKey string `json:"api_key"`
	}{key})
}

func (m *management) disableCustomer(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := requestContext(r)
	defer cancel()
	if err := m.store.DisableCustomer(ctx, r.PathValue("customerID")); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *management) putPolicy(w http.ResponseWriter, r *http.Request) {
	operation := r.PathValue("operation")
	if !validOperation(operation) {
		writeError(w, http.StatusBadRequest, "invalid operation")
		return
	}
	var body struct {
		DailyLimit         int64 `json:"daily_limit"`
		RateLimitPerMinute int32 `json:"rate_limit_per_minute"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.DailyLimit < 1 || body.DailyLimit > 1_000_000_000 || body.RateLimitPerMinute < 1 || body.RateLimitPerMinute > 1_000_000 {
		writeError(w, http.StatusBadRequest, "invalid policy limits")
		return
	}
	ctx, cancel := requestContext(r)
	defer cancel()
	policy, err := m.store.PutPolicy(ctx, store.Policy{
		CustomerID:         r.PathValue("customerID"),
		Operation:          operation,
		DailyLimit:         body.DailyLimit,
		RateLimitPerMinute: body.RateLimitPerMinute,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (m *management) me(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "query parameters are not accepted")
		return
	}
	customer, ok := m.customer(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, customer)
}

func (m *management) getPolicy(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "query parameters are not accepted")
		return
	}
	operation := r.PathValue("operation")
	if !validOperation(operation) {
		writeError(w, http.StatusBadRequest, "invalid operation")
		return
	}
	customer, ok := m.customer(w, r)
	if !ok {
		return
	}
	ctx, cancel := requestContext(r)
	defer cancel()
	policy, err := m.store.GetPolicy(ctx, customer.ID, operation)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (m *management) decide(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "query parameters are not accepted")
		return
	}
	customer, ok := m.customer(w, r)
	if !ok {
		return
	}
	var body struct {
		DecisionID string `json:"decision_id"`
		Operation  string `json:"operation"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if !decisionName.MatchString(body.DecisionID) || !validOperation(body.Operation) {
		writeError(w, http.StatusBadRequest, "invalid decision ID or operation")
		return
	}
	ctx, cancel := requestContext(r)
	defer cancel()
	decision, err := m.store.Decide(ctx, customer.ID, body.DecisionID, body.Operation)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			unauthorized(w)
		case errors.Is(err, store.ErrPolicyNotFound):
			writeError(w, http.StatusForbidden, "operation policy not found")
		case errors.Is(err, store.ErrDecisionConflict):
			writeError(w, http.StatusConflict, "decision ID already used for another operation")
		default:
			writeError(w, http.StatusServiceUnavailable, "decision storage unavailable")
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, decision)
}

func (m *management) usage(w http.ResponseWriter, r *http.Request) {
	customer, ok := m.customer(w, r)
	if !ok {
		return
	}
	params := r.URL.Query()
	operations := params["operation"]
	if len(params) != 1 || len(operations) != 1 || !validOperation(operations[0]) {
		writeError(w, http.StatusBadRequest, "exactly one valid operation is required")
		return
	}
	ctx, cancel := requestContext(r)
	defer cancel()
	usage, err := m.store.Usage(ctx, customer.ID, operations[0])
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			unauthorized(w)
		case errors.Is(err, store.ErrPolicyNotFound):
			writeError(w, http.StatusForbidden, "operation policy not found")
		default:
			writeError(w, http.StatusServiceUnavailable, "usage storage unavailable")
		}
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

func (m *management) customer(w http.ResponseWriter, r *http.Request) (store.Customer, bool) {
	token, ok := bearerToken(r)
	if !ok || len(token) != 67 || !strings.HasPrefix(token, "qg_") {
		unauthorized(w)
		return store.Customer{}, false
	}
	ctx, cancel := requestContext(r)
	defer cancel()
	customer, err := m.store.Authenticate(ctx, token)
	if errors.Is(err, store.ErrNotFound) {
		unauthorized(w)
		return store.Customer{}, false
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "customer storage unavailable")
		return store.Customer{}, false
	}
	return customer, true
}

func bearerToken(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) > 256 {
		return "", false
	}
	return parts[1], true
}

func validName(name string) bool {
	if n := utf8.RuneCountInString(name); n < 1 || n > 100 {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validOperation(name string) bool {
	return len(name) <= 64 && operationName.MatchString(name)
}

func decodeBody(w http.ResponseWriter, r *http.Request, into any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "one JSON object is required")
		return false
	}
	return true
}

func requestContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 3*time.Second)
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "unauthorized")
}

func writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeError(w, http.StatusServiceUnavailable, "storage unavailable")
}

func writeError(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, struct {
		Error string `json:"error"`
	}{message})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
