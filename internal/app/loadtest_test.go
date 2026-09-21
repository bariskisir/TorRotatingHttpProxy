package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoadTesterUsesProxyAndReportsResults(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Hostname() != "destination.invalid" {
			t.Error("test bypassed the configured HTTP proxy")
		}
		if !r.Close {
			t.Error("test requests must not reuse connections")
		}
		if calls.Add(1)%2 == 0 {
			http.Error(w, "empty pool", 503)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.ProxyAddr = strings.TrimPrefix(server.URL, "http://")
	tester := NewLoadTester(context.Background(), cfg)
	if err := tester.Start(TestOptions{URL: "http://destination.invalid/", RPS: 30}); err != nil {
		t.Fatal(err)
	}
	defer func() { tester.Stop(); tester.Wait() }()
	if err := tester.Start(TestOptions{URL: "http://destination.invalid/", RPS: 1}); err != errTestActive {
		t.Fatal("overlapping test was not rejected", err)
	}
	eventually(t, func() bool { s := tester.Snapshot(); return s.Success >= 2 && s.Unavailable >= 2 })
	tester.Stop()
	tester.Wait()
	s := tester.Snapshot()
	if s.State != "stopped" || s.InFlight != 0 || s.AverageMS <= 0 || s.SuccessAverageMS <= 0 || s.Failed != s.Unavailable || s.Sent != s.Success+s.Failed+s.Canceled {
		t.Fatalf("invalid final metrics: %+v", s)
	}
	if err := tester.Start(TestOptions{URL: "http://destination.invalid/", RPS: 1}); err != nil {
		t.Fatal("could not restart test", err)
	}
	tester.Stop()
	tester.Wait()
	if s := tester.Snapshot(); s.Success != 0 || s.Failed != 0 {
		t.Fatal("new run did not reset metrics")
	}
}

func TestLoadTesterCountsHTTPSConnect503(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" {
			t.Error("HTTPS test should use CONNECT")
		}
		http.Error(w, "empty pool", 503)
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.ProxyAddr = strings.TrimPrefix(server.URL, "http://")
	tester := NewLoadTester(context.Background(), cfg)
	if err := tester.Start(TestOptions{URL: "https://destination.invalid/", RPS: 20}); err != nil {
		t.Fatal(err)
	}
	defer func() { tester.Stop(); tester.Wait() }()
	eventually(t, func() bool { return tester.Snapshot().Unavailable >= 2 })
}

func TestLoadTesterStopCancelsInflightRequests(t *testing.T) {
	started := make(chan struct{}, 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { started <- struct{}{}; <-r.Context().Done() }))
	defer server.Close()
	cfg := testConfig()
	cfg.ProxyAddr = strings.TrimPrefix(server.URL, "http://")
	cfg.ConnectTimeout = 30 * time.Second
	tester := NewLoadTester(context.Background(), cfg)
	if err := tester.Start(TestOptions{URL: "http://destination.invalid/", RPS: 10}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("test did not issue a request")
	}
	tester.Stop()
	done := make(chan struct{})
	go func() { tester.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel active requests")
	}
	if s := tester.Snapshot(); s.Canceled == 0 || s.InFlight != 0 {
		t.Fatal(s)
	}
}

func TestLoadTesterRejectsInvalidOptions(t *testing.T) {
	tester := NewLoadTester(context.Background(), testConfig())
	for _, options := range []TestOptions{{"ftp://example.com", 1}, {"https://example.com", 0}, {"https://example.com", 1001}, {"https://user:pass@example.com", 1}, {"/relative", 1}} {
		if err := tester.Start(options); err == nil {
			tester.Stop()
			tester.Wait()
			t.Fatal("invalid options accepted", options)
		}
	}
}

func TestLoadTesterBoundsConcurrentRequests(t *testing.T) {
	var active, peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if old >= n || peak.CompareAndSwap(old, n) {
				break
			}
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.ProxyAddr = strings.TrimPrefix(server.URL, "http://")
	cfg.ConnectTimeout = 10 * time.Second
	tester := NewLoadTester(context.Background(), cfg)
	if err := tester.Start(TestOptions{URL: "http://destination.invalid/", RPS: 1000}); err != nil {
		t.Fatal(err)
	}
	defer func() { tester.Stop(); tester.Wait() }()
	eventually(t, func() bool { return tester.Snapshot().Skipped > 0 })
	if peak.Load() > 128 || tester.Snapshot().InFlight > 128 {
		t.Fatal("concurrency limit exceeded")
	}
}
