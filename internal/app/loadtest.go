package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

type TestOptions struct {
	URL string `json:"url"`
	RPS int    `json:"rps"`
}

type TestSnapshot struct {
	State            string         `json:"state"`
	URL              string         `json:"url"`
	TargetRPS        int            `json:"target_rps"`
	Started          time.Time      `json:"started"`
	Stopped          time.Time      `json:"stopped"`
	Sent             uint64         `json:"sent"`
	Success          uint64         `json:"success"`
	Failed           uint64         `json:"failed"`
	Unavailable      uint64         `json:"unavailable"`
	Canceled         uint64         `json:"canceled"`
	Skipped          uint64         `json:"skipped"`
	InFlight         int            `json:"in_flight"`
	AverageMS        float64        `json:"average_ms"`
	SuccessAverageMS float64        `json:"success_average_ms"`
	ActualRPS        float64        `json:"actual_rps"`
	LastError        string         `json:"last_error"`
	StatusCodes      map[int]uint64 `json:"status_codes"`
}

type LoadTester struct {
	mu          sync.Mutex
	parent      context.Context
	proxyURL    *url.URL
	timeout     time.Duration
	stats       TestSnapshot
	totalTime   time.Duration
	successTime time.Duration
	cancel      context.CancelFunc
	done        chan struct{}
	buckets     [10]rateBucket
}

type rateBucket struct {
	tick  int64
	count uint64
}

func NewLoadTester(ctx context.Context, cfg Config) *LoadTester {
	host, port, _ := net.SplitHostPort(cfg.ProxyAddr)
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return &LoadTester{parent: ctx, proxyURL: &url.URL{Scheme: "http", Host: net.JoinHostPort(host, port)}, timeout: cfg.ConnectTimeout,
		stats: TestSnapshot{State: "idle", URL: cfg.IPCheckURL, TargetRPS: 1, StatusCodes: map[int]uint64{}}}
}

var errTestActive = errors.New("a test is already running or stopping")

func (t *LoadTester) Start(options TestOptions) error {
	u, err := url.Parse(options.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("url must be an absolute HTTP(S) URL without credentials or a fragment")
	}
	if options.RPS < 1 || options.RPS > 1000 {
		return errors.New("rps must be an integer between 1 and 1000")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.parent.Err() != nil {
		return errors.New("application is shutting down")
	}
	if t.stats.State == "running" || t.stats.State == "stopping" {
		return errTestActive
	}
	ctx, cancel := context.WithCancel(t.parent)
	t.cancel = cancel
	t.done = make(chan struct{})
	t.stats = TestSnapshot{State: "running", URL: options.URL, TargetRPS: options.RPS, Started: time.Now(), StatusCodes: map[int]uint64{}}
	t.totalTime, t.successTime = 0, 0
	t.buckets = [10]rateBucket{}
	go t.run(ctx, options, t.done)
	return nil
}

func (t *LoadTester) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stats.State == "running" {
		t.stats.State = "stopping"
		t.cancel()
	}
}

func (t *LoadTester) Wait() {
	t.mu.Lock()
	done := t.done
	t.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (t *LoadTester) Snapshot() TestSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.stats
	s.StatusCodes = make(map[int]uint64, len(t.stats.StatusCodes))
	for code, count := range t.stats.StatusCodes {
		s.StatusCodes[code] = count
	}
	if n := s.Success + s.Failed; n > 0 {
		s.AverageMS = float64(t.totalTime) / float64(time.Millisecond) / float64(n)
	}
	if s.Success > 0 {
		s.SuccessAverageMS = float64(t.successTime) / float64(time.Millisecond) / float64(s.Success)
	}
	now := time.Now().UnixMilli() / 100
	for _, bucket := range t.buckets {
		if now-bucket.tick >= 0 && now-bucket.tick < 10 {
			s.ActualRPS += float64(bucket.count)
		}
	}
	return s
}

func (t *LoadTester) run(ctx context.Context, options TestOptions, done chan struct{}) {
	transport := &http.Transport{Proxy: http.ProxyURL(t.proxyURL), DisableKeepAlives: true, DisableCompression: true,
		DialContext: (&net.Dialer{Timeout: t.timeout}).DialContext, TLSHandshakeTimeout: t.timeout,
		ResponseHeaderTimeout: t.timeout, MaxResponseHeaderBytes: 64 * 1024}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: t.timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// Explicitly count proxy 503s during HTTPS CONNECT, which http.Client
	// otherwise exposes only as an opaque connection error.
	type connectCodeKey struct{}
	transport.OnProxyConnectResponse = func(ctx context.Context, _ *url.URL, _ *http.Request, resp *http.Response) error {
		if code, ok := ctx.Value(connectCodeKey{}).(*atomic.Int64); ok {
			code.Store(int64(resp.StatusCode))
		}
		return nil
	}
	var requests sync.WaitGroup
	slots := make(chan struct{}, 128)
	ticker := time.NewTicker(time.Second / time.Duration(options.RPS))
	defer ticker.Stop()
	defer func() {
		requests.Wait()
		t.mu.Lock()
		t.stats.State, t.stats.Stopped = "stopped", time.Now()
		t.cancel()
		close(done)
		t.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				return
			}
			select {
			case slots <- struct{}{}:
				t.mu.Lock()
				t.stats.Sent++
				t.stats.InFlight++
				t.mu.Unlock()
				requests.Add(1)
				go func() {
					defer requests.Done()
					defer func() { <-slots }()
					start := time.Now()
					var connectCode atomic.Int64
					reqCtx := context.WithValue(ctx, connectCodeKey{}, &connectCode)
					req, _ := http.NewRequestWithContext(reqCtx, http.MethodGet, options.URL, nil)
					resp, err := client.Do(req)
					code := 0
					if resp != nil {
						code = resp.StatusCode
						n, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 1024*1024+1))
						resp.Body.Close()
						if readErr != nil {
							err = readErr
						} else if n > 1024*1024 {
							err = errors.New("test response exceeds the 1 MiB limit")
						}
					} else if connectCode.Load() != 200 {
						code = int(connectCode.Load())
					}
					t.record(time.Since(start), code, err, ctx.Err() != nil)
				}()
			default:
				t.mu.Lock()
				t.stats.Skipped++
				t.mu.Unlock()
			}
		}
	}
}

func (t *LoadTester) record(elapsed time.Duration, code int, err error, canceled bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stats.InFlight--
	if canceled {
		t.stats.Canceled++
		return
	}
	t.totalTime += elapsed
	if code != 0 {
		t.stats.StatusCodes[code]++
	}
	if code == 503 {
		t.stats.Unavailable++
	}
	if err == nil && code >= 200 && code < 300 {
		t.stats.Success++
		t.successTime += elapsed
	} else {
		t.stats.Failed++
		if err != nil {
			t.stats.LastError = err.Error()
		} else {
			t.stats.LastError = "HTTP " + http.StatusText(code)
		}
	}
	tick := time.Now().UnixMilli() / 100
	i := tick % int64(len(t.buckets))
	if t.buckets[i].tick != tick {
		t.buckets[i] = rateBucket{tick: tick}
	}
	t.buckets[i].count++
}
