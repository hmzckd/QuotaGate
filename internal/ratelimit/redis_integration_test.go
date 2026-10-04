package ratelimit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func testLimiter(t *testing.T) *Limiter {
	t.Helper()
	addr := os.Getenv("QG_TEST_REDIS_URL")
	if addr == "" {
		t.Skip("set QG_TEST_REDIS_URL for real Redis integration tests")
	}
	l, err := New(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := l.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	return l
}

func testTenant(t *testing.T, l *Limiter) string {
	t.Helper()
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	customer := "test_" + hex.EncodeToString(random[:])
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var cursor uint64
		for {
			keys, next, err := l.redis.Scan(ctx, cursor, "qg:rate:"+hash(customer)+":*", 100).Result()
			if err != nil {
				t.Errorf("scan test customer's Redis keys: %v", err)
				return
			}
			if len(keys) > 0 {
				if err := l.redis.Del(ctx, keys...).Err(); err != nil {
					t.Errorf("clean test customer's Redis keys: %v", err)
				}
			}
			cursor = next
			if cursor == 0 {
				return
			}
		}
	})
	return customer
}

func safeWindow(t *testing.T, l *Limiter) {
	t.Helper()
	now, err := l.redis.Time(context.Background()).Result()
	if err != nil {
		t.Fatal(err)
	}
	remaining := 60_000 - now.UnixMilli()%60_000
	if remaining < 5_000 {
		time.Sleep(time.Duration(remaining)*time.Millisecond + 30*time.Millisecond)
	}
}

func TestRedisTwoClientsShareConcurrentLimit(t *testing.T) {
	a, b := testLimiter(t), testLimiter(t)
	customer := testTenant(t, a)
	safeWindow(t, a)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	results := make(chan Result, 60)
	errorsCh := make(chan error, 60)
	var wg sync.WaitGroup
	for i := range 60 {
		wg.Go(func() {
			l := a
			if i%2 == 1 {
				l = b
			}
			result, err := l.Check(ctx, customer, fmt.Sprintf("concurrent_%020d", i), "job.create", 7)
			if err != nil {
				errorsCh <- err
			} else {
				results <- result
			}
		})
	}
	wg.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		t.Error(err)
	}
	allowed, denied := 0, 0
	var window string
	for result := range results {
		if window == "" {
			window = result.WindowStartUTC
		}
		if result.WindowStartUTC != window {
			t.Fatal("test unexpectedly crossed a real window boundary")
		}
		if result.Allowed && result.Reason == "rate_allowed" {
			allowed++
		} else if !result.Allowed && result.Reason == "rate_limited" && result.RateUsed == 7 {
			denied++
		} else {
			t.Errorf("unexpected rate result: %+v", result)
		}
	}
	if allowed != 7 || denied != 53 {
		t.Fatalf("allowed/denied = %d/%d, want 7/53", allowed, denied)
	}
	used, err := a.redis.HGet(ctx, rateKeys(customer, "unused_decision_id", "job.create")[0], "used").Int64()
	if err != nil || used != 7 {
		t.Fatalf("Redis used = %d, error = %v", used, err)
	}
}

func TestRedisConcurrentReplaysKeepFirstResult(t *testing.T) {
	a, b := testLimiter(t), testLimiter(t)
	customer := testTenant(t, a)
	safeWindow(t, a)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const id = "duplicate_decision_0001"
	first, err := a.Check(ctx, customer, id, "job.create", 1)
	if err != nil || !first.Allowed {
		t.Fatalf("first result = %+v, err = %v", first, err)
	}
	keys := rateKeys(customer, id, "job.create")
	before := a.redis.PTTL(ctx, keys[1]).Val()
	time.Sleep(20 * time.Millisecond)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			result, err := b.Check(ctx, customer, id, "job.create", 100)
			if err != nil || result != first {
				t.Errorf("replay = %+v, err = %v, want %+v", result, err, first)
			}
		})
	}
	wg.Wait()
	if after := a.redis.PTTL(ctx, keys[1]).Val(); after >= before {
		t.Error("replay extended the 24-hour pre-result lifetime")
	}
	denied, err := a.Check(ctx, customer, "denied_decision_0001", "job.create", 1)
	if err != nil || denied.Allowed || denied.RateUsed != 1 {
		t.Fatalf("denial = %+v, err = %v", denied, err)
	}
	replayed, err := b.Check(ctx, customer, "denied_decision_0001", "job.create", 100)
	if err != nil || replayed != denied {
		t.Fatalf("denial replay = %+v, err = %v", replayed, err)
	}
	if _, err := b.Check(ctx, customer, id, "job.other", 10); !errors.Is(err, ErrDecisionConflict) {
		t.Fatalf("different operation error = %v", err)
	}
	used, err := a.redis.HGet(ctx, keys[0], "used").Int64()
	if err != nil || used != 1 {
		t.Fatalf("replays/denial changed rate capacity: used = %d, err = %v", used, err)
	}
}

func TestRedisCustomerOperationIsolationAndScriptReload(t *testing.T) {
	l := testLimiter(t)
	a, b := testTenant(t, l), testTenant(t, l)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, input := range []struct{ customer, id, operation string }{
		{a, "same_decision_000001", "job.create"},
		{b, "same_decision_000001", "job.create"},
		{a, "other_decision_00001", "job.other"},
	} {
		result, err := l.Check(ctx, input.customer, input.id, input.operation, 1)
		if err != nil || !result.Allowed || result.RateUsed != 1 {
			t.Fatalf("isolated rate result = %+v, err = %v", result, err)
		}
	}
	// The dedicated test Redis owns this script cache; no unrelated server is used.
	if err := l.redis.ScriptFlush(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	result, err := l.Check(ctx, a, "same_decision_000001", "job.create", 1)
	if err != nil || !result.Allowed || result.RateUsed != 1 {
		t.Fatalf("NOSCRIPT fallback replay = %+v, err = %v", result, err)
	}
}

func TestRedisTimeAndKeyExpiry(t *testing.T) {
	l := testLimiter(t)
	customer := testTenant(t, l)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	before, err := l.redis.Time(ctx).Result()
	if err != nil {
		t.Fatal(err)
	}
	result, err := l.Check(ctx, customer, "expiry_decision_0001", "job.create", 2)
	if err != nil {
		t.Fatal(err)
	}
	after, err := l.redis.Time(ctx).Result()
	if err != nil {
		t.Fatal(err)
	}
	start, err := time.Parse(time.RFC3339, result.WindowStartUTC)
	if err != nil || start.Before(before.Truncate(time.Minute)) || start.After(after.Truncate(time.Minute)) {
		t.Fatalf("window is not based on Redis TIME: %+v", result)
	}
	end, err := time.Parse(time.RFC3339, result.ResetsAtUTC)
	if err != nil || end.Sub(start) != time.Minute {
		t.Fatalf("invalid fixed window: %+v", result)
	}
	keys := rateKeys(customer, "expiry_decision_0001", "job.create")
	expiresAt, err := l.redis.Do(ctx, "PEXPIRETIME", keys[0]).Int64()
	if err != nil || expiresAt != end.UnixMilli() {
		t.Fatalf("bucket expiry = %d, err = %v, want %d", expiresAt, err, end.UnixMilli())
	}
	if ttl := l.redis.PTTL(ctx, keys[1]).Val(); ttl < 24*time.Hour-time.Minute || ttl > 24*time.Hour {
		t.Fatalf("pre-result TTL = %s, want approximately 24 hours", ttl)
	}
}

func TestRedisWindowBoundaryWithControlledLuaClock(t *testing.T) {
	l := testLimiter(t)
	customer := testTenant(t, l)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now, err := l.redis.Time(ctx).Result()
	if err != nil {
		t.Fatal(err)
	}
	// Only the TIME expression changes in the real production Lua script. Both
	// selected windows lie in the future so PEXPIREAT is still exercised in Redis.
	base := now.UTC().Truncate(time.Minute).Add(time.Minute)
	clockLine := "local now = redis.call('TIME')"
	if strings.Count(rateScript, clockLine) != 1 {
		t.Fatal("production Lua clock expression changed")
	}
	call := func(id string, at time.Time) Result {
		t.Helper()
		clock := fmt.Sprintf("local now = {%d, %d}", at.Unix(), at.Nanosecond()/1000)
		script := strings.Replace(rateScript, clockLine, clock, 1)
		values, err := l.redis.Eval(ctx, script, rateKeys(customer, id, "job.create"), "job.create", 1).Int64Slice()
		if err != nil {
			t.Fatal(err)
		}
		result, err := decodeResult(values)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	lastMS := base.Add(time.Minute - time.Millisecond)
	first := call("boundary_allowed_001", lastMS)
	denied := call("boundary_denied_0001", lastMS)
	next := call("boundary_allowed_002", lastMS.Add(time.Millisecond))
	if !first.Allowed || denied.Allowed || !next.Allowed || next.RateUsed != 1 ||
		first.ResetsAtUTC != next.WindowStartUTC {
		t.Fatalf("boundary results: first=%+v denied=%+v next=%+v", first, denied, next)
	}
	if replay := call("boundary_allowed_001", base.Add(time.Minute)); replay != first {
		t.Fatalf("old allowed result changed across window: %+v", replay)
	}
	if replay := call("boundary_denied_0001", base.Add(time.Minute)); replay != denied {
		t.Fatalf("old denied result changed across window: %+v", replay)
	}
	used, err := l.redis.HGet(ctx, rateKeys(customer, "boundary_allowed_002", "job.create")[0], "used").Int64()
	if err != nil || used != 1 {
		t.Fatalf("old replays changed next-window usage: %d, err=%v", used, err)
	}
}
