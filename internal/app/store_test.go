package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "ips.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testConfig() Config {
	return Config{TorCount: 2, UniqueIP: true, MaxUsedIPRetries: 3, ConnectTimeout: time.Second,
		IdleTimeout: time.Second, IPCheckURL: "https://api.ipify.org", ProxyAddr: ":3128", WebAddr: ":8080"}
}

func testEndpoint(t *testing.T, id int, ip string) *endpoint {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &endpoint{id: id, ip: ip, circuit: "7", token: "abcd", ctx: ctx, cancel: cancel, done: make(chan struct{}),
		dial: (&net.Dialer{}).DialContext}
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true before timeout")
}

func TestStoreAtomicConsumeAndPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ips.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if unused, err := s.Observe(ctx, "8.8.8.8"); err != nil || !unused {
		t.Fatalf("observe: %v %v", unused, err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.Consume(ctx, "8.8.8.8", true)
			if err != nil {
				t.Error(err)
			}
			if ok {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("IP consumed %d times", wins.Load())
	}
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if unused, err := s.Observe(ctx, "8.8.8.8"); err != nil || unused {
		t.Fatalf("used flag lost after restart: %v %v", unused, err)
	}
	for range 2 {
		if ok, err := s.Consume(ctx, "8.8.8.8", false); err != nil || !ok {
			t.Fatalf("reuse should succeed: %v %v", ok, err)
		}
	}
	if _, err := s.Observe(ctx, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	page, err := s.List(ctx, "8.8", "true", 1, 1)
	if err != nil || page.Total != 1 || len(page.Items) != 1 || !page.Items[0].Used {
		t.Fatalf("unexpected page: %+v %v", page, err)
	}
	var columns int
	if err := s.db.QueryRow("SELECT count(*) FROM pragma_table_info('ips')").Scan(&columns); err != nil || columns != 2 {
		t.Fatalf("expected exactly two columns: %d %v", columns, err)
	}
}

func TestPoolUniqueReservationAndBusyExclusion(t *testing.T) {
	s := testStore(t)
	p := NewPool(testConfig(), s)
	a, b := testEndpoint(t, 1, "8.8.8.8"), testEndpoint(t, 2, "8.8.8.8")
	if ok, err := p.publish(context.Background(), a); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, err := p.publish(context.Background(), b); ok || err != nil {
		t.Fatal("duplicate ready IP was accepted", ok, err)
	}
	var wg sync.WaitGroup
	var leases atomic.Int32
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := p.Acquire(context.Background(), 0)
			if err == nil {
				leases.Add(1)
				lease.Finish(errors.New("simulated failure"))
			} else if !errors.Is(err, ErrNoReady) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if leases.Load() != 1 {
		t.Fatal("one ready IP must produce exactly one lease")
	}
	if _, err := p.publish(context.Background(), b); !errors.Is(err, errUsedIP) {
		t.Fatalf("expected used IP error, got %v", err)
	}
	total, used, err := s.Counts(context.Background())
	if err != nil || total != 1 || used != 1 {
		t.Fatal(total, used, err)
	}
}

func TestPoolReuseRequiresExplicitRepublish(t *testing.T) {
	cfg := testConfig()
	cfg.UniqueIP = false
	p := NewPool(cfg, testStore(t))
	for id := 1; id <= 2; id++ {
		if ok, err := p.publish(context.Background(), testEndpoint(t, id, "8.8.8.8")); !ok || err != nil {
			t.Fatal(ok, err)
		}
	}
	for range 2 {
		lease, err := p.Acquire(context.Background(), 0)
		if err != nil {
			t.Fatal(err)
		}
		lease.Finish(nil)
	}
	if _, err := p.Acquire(context.Background(), 0); !errors.Is(err, ErrNoReady) {
		t.Fatalf("finished instances cannot be reused without rotation: %v", err)
	}
	if ok, err := p.publish(context.Background(), testEndpoint(t, 1, "8.8.8.8")); !ok || err != nil {
		t.Fatal(ok, err)
	}
	lease, err := p.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	lease.Finish(nil)
}

func TestPoolClosedCircuitAndDatabaseFailure(t *testing.T) {
	s := testStore(t)
	p := NewPool(testConfig(), s)
	a := testEndpoint(t, 1, "8.8.8.8")
	if ok, err := p.publish(context.Background(), a); !ok || err != nil {
		t.Fatal(ok, err)
	}
	a.cancel()
	if p.Ready() {
		t.Fatal("canceled circuit must not be ready")
	}
	if _, err := p.Acquire(context.Background(), 0); !errors.Is(err, ErrNoReady) {
		t.Fatal(err)
	}
	b := testEndpoint(t, 2, "1.1.1.1")
	if ok, err := p.publish(context.Background(), b); !ok || err != nil {
		t.Fatal(ok, err)
	}
	s.Close()
	if lease, err := p.Acquire(context.Background(), 0); err == nil || lease != nil {
		t.Fatal("reservation must fail when durable storage is unavailable")
	}
}

func TestUsedIPRestartPolicy(t *testing.T) {
	cfg := testConfig()
	streak := 0
	for n := 1; n <= 3; n++ {
		if got := shouldRestartForUsedIP(cfg, &streak, errUsedIP); got != (n == 3) {
			t.Fatalf("attempt %d: restart=%v", n, got)
		}
	}
	shouldRestartForUsedIP(cfg, &streak, nil)
	if streak != 0 {
		t.Fatal("a successful cycle should reset the streak")
	}
	shouldRestartForUsedIP(cfg, &streak, errUsedIP)
	shouldRestartForUsedIP(cfg, &streak, errors.New("network error"))
	if streak != 0 {
		t.Fatal("unrelated failures must break a consecutive used-IP streak")
	}
	cfg.UniqueIP = false
	for range 10 {
		if shouldRestartForUsedIP(cfg, &streak, errUsedIP) {
			t.Fatal("reuse mode must not restart because of duplicate IPs")
		}
	}
	cfg.UniqueIP = true
	cfg.MaxUsedIPRetries = 1
	if !shouldRestartForUsedIP(cfg, &streak, errUsedIP) {
		t.Fatal("configured threshold was ignored")
	}
}

func TestFreshResetOnlyRemovesOneInstance(t *testing.T) {
	root := t.TempDir()
	for _, relative := range []string{"tor/1/state", "tor/2/state", "ips.db"} {
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := resetInstanceData(root, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "tor/1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("instance data remains")
	}
	for _, path := range []string{"tor/2/state", "ips.db"} {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Fatal(err)
		}
	}
	if err := resetInstanceData(root, 0); err == nil {
		t.Fatal("invalid instance reset accepted")
	}
}

func TestPoolStateClearsStaleError(t *testing.T) {
	p := NewPool(testConfig(), testStore(t))
	p.state(1, "Error", 0, errors.New("bootstrap: context deadline exceeded"))
	if p.slots[0].view.LastError == "" {
		t.Fatal("error was not recorded")
	}
	p.state(1, "Bootstrapping", 0, nil)
	if view := p.slots[0].view; view.LastError != "" || view.Bootstrap != 0 || view.State != "Bootstrapping" {
		t.Fatalf("stale error was not cleared: %+v", view)
	}
}

func TestNeedsFreshRestart(t *testing.T) {
	if !needsFreshRestart(fmt.Errorf("bootstrap did not reach 100%%: %w", errBootstrapStuck)) {
		t.Fatal("stuck bootstrap must rebuild the instance")
	}
	if !needsFreshRestart(errFreshRestart) {
		t.Fatal("used-IP limit must rebuild the instance")
	}
	if !needsFreshRestart(fmt.Errorf("serve: %w", errServeFailures)) {
		t.Fatal("consecutive serving failures must rebuild the instance")
	}
	if needsFreshRestart(errors.New("network error")) {
		t.Fatal("ordinary errors must not discard Tor state")
	}
}

func TestAcquireWaitsForReadyInstance(t *testing.T) {
	p := NewPool(testConfig(), testStore(t))
	done := make(chan *Lease, 1)
	go func() {
		lease, err := p.Acquire(context.Background(), 2*time.Second)
		if err != nil {
			done <- nil
			return
		}
		done <- lease
	}()
	time.Sleep(100 * time.Millisecond)
	if ok, err := p.publish(context.Background(), testEndpoint(t, 1, "8.8.8.8")); !ok || err != nil {
		t.Fatal(ok, err)
	}
	select {
	case lease := <-done:
		if lease == nil {
			t.Fatal("waiting acquire failed")
		}
		lease.Finish(nil)
	case <-time.After(3 * time.Second):
		t.Fatal("acquire did not wait for the ready instance")
	}
}

func TestAcquireTimesOutWhenPoolStaysEmpty(t *testing.T) {
	p := NewPool(testConfig(), testStore(t))
	start := time.Now()
	if _, err := p.Acquire(context.Background(), 200*time.Millisecond); !errors.Is(err, ErrNoReady) {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatal("acquire did not wait for the full budget")
	}
	s, _ := p.Snapshot(context.Background())
	if s.Rejected != 1 {
		t.Fatal("timeout was not counted as rejected")
	}
}

func TestConsecutiveFailuresAreTracked(t *testing.T) {
	cfg := testConfig()
	cfg.UniqueIP = false
	p := NewPool(cfg, testStore(t))
	for i := 0; i < maxConsecutiveServeFailures; i++ {
		ep := testEndpoint(t, 1, fmt.Sprintf("9.9.9.%d", i+1))
		if ok, err := p.publish(context.Background(), ep); !ok || err != nil {
			t.Fatal(ok, err)
		}
		lease, err := p.Acquire(context.Background(), 0)
		if err != nil {
			t.Fatal(err)
		}
		lease.Finish(errors.New("boom"))
		if got := p.consecutiveFailures(1); got != i+1 {
			t.Fatalf("failures=%d, want %d", got, i+1)
		}
		if ep.lastErr == nil {
			t.Fatal("serving result was not recorded on the endpoint")
		}
	}
	ep := testEndpoint(t, 1, "9.9.9.9")
	if ok, err := p.publish(context.Background(), ep); !ok || err != nil {
		t.Fatal(ok, err)
	}
	lease, err := p.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	lease.Finish(nil)
	if got := p.consecutiveFailures(1); got != 0 {
		t.Fatalf("success did not reset the streak: %d", got)
	}
}

func TestConfigDefaultsAndValidation(t *testing.T) {
	t.Setenv("TOR_COUNT", "10")
	t.Setenv("UNIQUE_IP", "true")
	t.Setenv("MAX_USED_IP_RETRIES", "3")
	cfg, err := LoadConfig()
	if err != nil || cfg.TorCount != 10 || !cfg.UniqueIP || cfg.MaxUsedIPRetries != 3 {
		t.Fatal(cfg, err)
	}
	for _, entry := range []struct{ name, value string }{{"TOR_COUNT", "0"}, {"UNIQUE_IP", "yes"}, {"MAX_USED_IP_RETRIES", "-1"}, {"CONNECT_TIMEOUT", "forever"}, {"IP_CHECK_URL", "ftp://example.com"}, {"WEB_ADDR", ":70000"}} {
		t.Run(entry.name, func(t *testing.T) {
			t.Setenv(entry.name, entry.value)
			if _, err := LoadConfig(); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}
