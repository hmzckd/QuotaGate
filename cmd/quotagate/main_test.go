package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestServeFinishesInFlightRequestBeforeShutdown(t *testing.T) {
	started := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte("done"))
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- serve(ctx, newServer(handler), listener) }()

	body := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/")
		if err != nil {
			body <- "error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		body <- string(b)
	}()
	<-started
	cancel() // like SIGTERM while a request is running

	if got := <-body; got != "done" {
		t.Fatalf("in-flight request got %q, want it to finish", got)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(shutdownTimeout):
		t.Fatal("shutdown did not finish")
	}
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		conn.Close()
		t.Fatal("server still accepts connections after shutdown")
	}
}

func TestServerTimeoutsCoverRequestBudget(t *testing.T) {
	s := newServer(http.NotFoundHandler())
	if s.ReadHeaderTimeout == 0 || s.ReadTimeout == 0 || s.WriteTimeout == 0 || s.IdleTimeout == 0 {
		t.Fatal("every server timeout must be set")
	}
	// The handler's 4s budget must end before the write deadline, and shutdown
	// must wait longer than any request can run.
	if s.WriteTimeout <= 4*time.Second || shutdownTimeout <= s.WriteTimeout {
		t.Fatalf("write=%s shutdown=%s do not cover the 4s request budget", s.WriteTimeout, shutdownTimeout)
	}
}

func TestPruneDecisionsRefusesUnsafeArgumentsBeforeConnecting(t *testing.T) {
	// The URL is never dialed: every case must fail before the pool is opened.
	const unusedURL = "postgres://nobody@127.0.0.1:1/none?sslmode=disable"
	cases := map[string][]string{
		"younger than 48h":        {"-older-than", "47h"},
		"apply without the dash":  {"apply"},
		"flag after a stray word": {"now", "-apply"},
		"not a duration":          {"-older-than", "3d"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			err := pruneDecisions(context.Background(), unusedURL, args)
			if err == nil {
				t.Fatal("unsafe arguments were accepted")
			}
			if strings.Contains(err.Error(), "database") || strings.Contains(err.Error(), "connect") {
				t.Fatalf("command reached the database: %v", err)
			}
		})
	}
}
