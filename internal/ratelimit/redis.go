package ratelimit

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

//go:embed rate.lua
var rateScript string

var ErrDecisionConflict = errors.New("rate decision ID already used for another operation")

type Result struct {
	Allowed        bool   `json:"allowed"`
	Reason         string `json:"reason"`
	RateUsed       int64  `json:"rate_used"`
	RateLimit      int64  `json:"rate_limit"`
	WindowStartUTC string `json:"window_start_utc"`
	ResetsAtUTC    string `json:"resets_at_utc"`
}

type Limiter struct {
	redis  *redis.Client
	script *redis.Script
}

func New(redisURL string) (*Limiter, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil || (opts.Network != "tcp") {
		return nil, errors.New("REDIS_URL must be a valid redis:// or rediss:// URL")
	}
	opts.Protocol = 2
	opts.PoolSize = 10
	opts.MaxActiveConns = 10
	opts.MaxRetries = -1
	opts.DialTimeout = time.Second
	opts.ReadTimeout = time.Second
	opts.WriteTimeout = time.Second
	opts.PoolTimeout = time.Second
	opts.ContextTimeoutEnabled = true
	return &Limiter{redis.NewClient(opts), redis.NewScript(rateScript)}, nil
}

func (l *Limiter) Close() error { return l.redis.Close() }

func (l *Limiter) Ping(ctx context.Context) error {
	if err := l.redis.Ping(ctx).Err(); err != nil {
		return errors.New("Redis unavailable")
	}
	return nil
}

// Check reserves only Redis rate capacity. It does not change daily usage or
// grant permission to run an application handler; QG-06 adds the final decision.
func (l *Limiter) Check(ctx context.Context, customerID, decisionID, operation string, limit int64) (Result, error) {
	if customerID == "" || len(customerID) > 128 || len(decisionID) < 16 || len(decisionID) > 128 ||
		operation == "" || len(operation) > 64 || limit < 1 || limit > 1_000_000 {
		return Result{}, errors.New("invalid rate check input")
	}
	keys := rateKeys(customerID, decisionID, operation)
	values, err := l.script.Run(ctx, l.redis, keys, operation, limit).Int64Slice()
	if err != nil {
		if strings.TrimPrefix(err.Error(), "ERR ") == "QG_DECISION_CONFLICT" {
			return Result{}, ErrDecisionConflict
		}
		return Result{}, errors.New("Redis rate check unavailable")
	}
	return decodeResult(values)
}

func rateKeys(customerID, decisionID, operation string) []string {
	customer := "qg:rate:" + hash(customerID)
	return []string{customer + ":bucket:" + hash(operation), customer + ":decision:" + hash(decisionID)}
}

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func decodeResult(values []int64) (Result, error) {
	if len(values) != 5 || (values[0] != 0 && values[0] != 1) || values[1] < 0 || values[2] < 1 ||
		values[3] < 0 || values[3]%60_000 != 0 || values[4]-values[3] != 60_000 {
		return Result{}, errors.New("invalid Redis rate result")
	}
	result := Result{
		Allowed:        values[0] == 1,
		Reason:         "rate_limited",
		RateUsed:       values[1],
		RateLimit:      values[2],
		WindowStartUTC: time.UnixMilli(values[3]).UTC().Format(time.RFC3339),
		ResetsAtUTC:    time.UnixMilli(values[4]).UTC().Format(time.RFC3339),
	}
	if result.Allowed {
		if result.RateUsed < 1 || result.RateUsed > result.RateLimit {
			return Result{}, errors.New("inconsistent Redis rate allowance")
		}
		result.Reason = "rate_allowed"
	}
	return result, nil
}
