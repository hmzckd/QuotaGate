package ratelimit

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

func TestCheckGivesUpOnSilentRedis(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	defer func() {
		listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			conn.Close()
		}
	}()

	limiter, err := New("redis://" + listener.Addr().String() + "/0")
	if err != nil {
		t.Fatal(err)
	}
	defer limiter.Close()
	start := time.Now()
	_, err = limiter.Check(context.Background(), "customer-1", "silent_decision_0001", "job.create", 5)
	if err == nil {
		t.Fatal("check against a silent Redis succeeded")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("check took %s; the 1s read timeout should end it", elapsed)
	}
}
