package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func Run(ctx context.Context, cfg Config) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(cfg.DataDir, "app.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("data directory is already used by another application: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	store, err := OpenStore(filepath.Join(cfg.DataDir, "ips.db"))
	if err != nil {
		return fmt.Errorf("open SQLite: %w", err)
	}
	defer store.Close()
	runtimeDir, err := os.MkdirTemp("", "torproxy-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(runtimeDir)
	proxyListener, err := net.Listen("tcp", cfg.ProxyAddr)
	if err != nil {
		return err
	}
	defer proxyListener.Close()
	webListener, err := net.Listen("tcp", cfg.WebAddr)
	if err != nil {
		return err
	}
	defer webListener.Close()
	pool := NewPool(cfg, store)
	tester := NewLoadTester(ctx, cfg)
	proxySockets := newBoundedListener(proxyListener, cfg.MaxConnections, cfg.IdleTimeout)
	webSockets := newBoundedListener(webListener, 128, cfg.IdleTimeout)
	proxyServer := &http.Server{Handler: &ProxyHandler{Pool: pool, Config: cfg}, ReadHeaderTimeout: 15 * time.Second, IdleTimeout: cfg.IdleTimeout, MaxHeaderBytes: 64 * 1024, BaseContext: func(net.Listener) context.Context { return ctx }}
	webServer := &http.Server{Handler: WebHandler(pool, store, tester), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: cfg.IdleTimeout, MaxHeaderBytes: 16 * 1024, BaseContext: func(net.Listener) context.Context { return ctx }}
	workers := RunWorkers(ctx, cfg, pool, runtimeDir)
	serverErrors := make(chan error, 2)
	go func() { serverErrors <- proxyServer.Serve(proxySockets) }()
	go func() { serverErrors <- webServer.Serve(webSockets) }()
	slog.Info("TorRotatingHttpProxy started", "proxy", cfg.ProxyAddr, "dashboard", cfg.WebAddr, "tor_count", cfg.TorCount, "unique_ip", cfg.UniqueIP, "max_used_ip_retries", cfg.MaxUsedIPRetries)
	select {
	case <-ctx.Done():
	case err = <-serverErrors:
	}
	cancel()
	// Close hijacked sockets too; net/http Shutdown does not own them.
	proxyServer.Close()
	webServer.Close()
	proxySockets.closeConnections()
	webSockets.closeConnections()
	workers.Wait()
	tester.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func Healthcheck(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("healthcheck returned %s", resp.Status)
	}
	return nil
}
