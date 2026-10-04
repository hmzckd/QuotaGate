package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("customer or policy not found")

type Customer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Policy struct {
	CustomerID         string `json:"customer_id"`
	Operation          string `json:"operation"`
	DailyLimit         int64  `json:"daily_limit"`
	RateLimitPerMinute int32  `json:"rate_limit_per_minute"`
	Version            int64  `json:"version"`
}

type Store struct {
	db    *pgxpool.Pool
	clock dbClock
}

func New(db *pgxpool.Pool) *Store { return &Store{db: db} }

func (s *Store) CreateCustomer(ctx context.Context, name string) (Customer, string, error) {
	id, err := randomHex(16)
	if err != nil {
		return Customer{}, "", err
	}
	key, err := newKey()
	if err != nil {
		return Customer{}, "", err
	}
	customer := Customer{ID: "cus_" + id, Name: name}
	hash := sha256.Sum256([]byte(key))
	_, err = s.db.Exec(ctx, `INSERT INTO quotagate.customers (id, name, api_key_hash) VALUES ($1, $2, $3)`, customer.ID, customer.Name, hash[:])
	if err != nil {
		return Customer{}, "", fmt.Errorf("create customer: %w", err)
	}
	return customer, key, nil
}

func (s *Store) Authenticate(ctx context.Context, key string) (Customer, error) {
	hash := sha256.Sum256([]byte(key))
	var customer Customer
	err := s.db.QueryRow(ctx, `SELECT id, name FROM quotagate.customers WHERE api_key_hash = $1 AND disabled_at IS NULL`, hash[:]).Scan(&customer.ID, &customer.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return Customer{}, ErrNotFound
	}
	if err != nil {
		return Customer{}, fmt.Errorf("authenticate customer: %w", err)
	}
	return customer, nil
}

func (s *Store) RotateKey(ctx context.Context, customerID string) (string, error) {
	key, err := newKey()
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(key))
	var id string
	err = s.db.QueryRow(ctx, `UPDATE quotagate.customers SET api_key_hash = $2 WHERE id = $1 AND disabled_at IS NULL RETURNING id`, customerID, hash[:]).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("rotate key: %w", err)
	}
	return key, nil
}

func (s *Store) DisableCustomer(ctx context.Context, customerID string) error {
	var id string
	err := s.db.QueryRow(ctx, `UPDATE quotagate.customers SET disabled_at = COALESCE(disabled_at, now()) WHERE id = $1 RETURNING id`, customerID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("disable customer: %w", err)
	}
	return nil
}

func (s *Store) PutPolicy(ctx context.Context, policy Policy) (Policy, error) {
	var saved Policy
	err := s.db.QueryRow(ctx, `
		INSERT INTO quotagate.policies AS p (customer_id, operation, daily_limit, rate_limit_per_minute)
		SELECT id, $2, $3, $4 FROM quotagate.customers WHERE id = $1 AND disabled_at IS NULL
		ON CONFLICT (customer_id, operation) DO UPDATE SET
			daily_limit = EXCLUDED.daily_limit,
			rate_limit_per_minute = EXCLUDED.rate_limit_per_minute,
			version = p.version + 1,
			updated_at = now()
		RETURNING customer_id, operation, daily_limit, rate_limit_per_minute, version`,
		policy.CustomerID, policy.Operation, policy.DailyLimit, policy.RateLimitPerMinute,
	).Scan(&saved.CustomerID, &saved.Operation, &saved.DailyLimit, &saved.RateLimitPerMinute, &saved.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return Policy{}, ErrNotFound
	}
	if err != nil {
		return Policy{}, fmt.Errorf("put policy: %w", err)
	}
	return saved, nil
}

func (s *Store) GetPolicy(ctx context.Context, customerID, operation string) (Policy, error) {
	var policy Policy
	err := s.db.QueryRow(ctx, `
		SELECT p.customer_id, p.operation, p.daily_limit, p.rate_limit_per_minute, p.version
		FROM quotagate.policies p
		JOIN quotagate.customers c ON c.id = p.customer_id
		WHERE p.customer_id = $1 AND p.operation = $2 AND c.disabled_at IS NULL`, customerID, operation,
	).Scan(&policy.CustomerID, &policy.Operation, &policy.DailyLimit, &policy.RateLimitPerMinute, &policy.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return Policy{}, ErrNotFound
	}
	if err != nil {
		return Policy{}, fmt.Errorf("get policy: %w", err)
	}
	return policy, nil
}

func newKey() (string, error) {
	value, err := randomHex(32)
	if err != nil {
		return "", err
	}
	return "qg_" + value, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}
