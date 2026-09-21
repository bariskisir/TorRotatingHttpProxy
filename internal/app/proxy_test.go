package app

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func proxyFixture(t *testing.T) (*Pool, *http.Client, *httptest.Server) {
	t.Helper()
	cfg := testConfig()
	p := NewPool(cfg, testStore(t))
	if ok, err := p.publish(context.Background(), testEndpoint(t, 1, "8.8.8.8")); !ok || err != nil {
		t.Fatal(ok, err)
	}
	server := httptest.NewServer(&ProxyHandler{Pool: p, Config: cfg})
	t.Cleanup(server.Close)
	proxyURL, _ := url.Parse(server.URL)
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	t.Cleanup(transport.CloseIdleConnections)
	return p, &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, server
}

func TestHTTPForwardBodyHeadersAndRotation(t *testing.T) {
	p, client, _ := proxyFixture(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("X-Remove") != "" || r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("X-Keep") != "yes" {
			t.Error("request method/headers changed incorrectly")
		}
		data, _ := io.ReadAll(r.Body)
		w.Header().Set("Proxy-Authenticate", "secret")
		w.Header().Set("X-Upstream", "yes")
		w.Header().Set("Trailer", "X-Final")
		w.WriteHeader(201)
		w.Write(data)
		w.Header().Set("X-Final", "done")
	}))
	defer target.Close()
	req, _ := http.NewRequest("POST", target.URL, strings.NewReader("payload"))
	req.Header.Set("Connection", "X-Remove")
	req.Header.Set("X-Remove", "secret")
	req.Header.Set("Proxy-Authorization", "ignored")
	req.Header.Set("X-Keep", "yes")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(body) != "payload" || resp.StatusCode != 201 || resp.Header.Get("Proxy-Authenticate") != "" || resp.Header.Get("X-Upstream") != "yes" || resp.Trailer.Get("X-Final") != "done" {
		t.Fatalf("response: %s %+v %v", body, resp, err)
	}
	eventually(t, func() bool {
		s, _ := p.Snapshot(context.Background())
		return s.Completed == 1 && s.States["Rotating"] == 1
	})
	resp, err = client.Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatal("consumed instance was reused before rotation")
	}
}

func TestStreamingResponseKeepsInstanceBusy(t *testing.T) {
	p, client, _ := proxyFixture(t)
	finish := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "a")
		w.(http.Flusher).Flush()
		<-finish
		fmt.Fprint(w, "b")
	}))
	defer target.Close()
	resp, err := client.Get(target.URL)
	if err != nil {
		close(finish)
		t.Fatal(err)
	}
	one := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, one); err != nil {
		close(finish)
		t.Fatal(err)
	}
	s, _ := p.Snapshot(context.Background())
	if s.States["Busy"] != 1 {
		close(finish)
		t.Fatal("rotated before body completed")
	}
	close(finish)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	eventually(t, func() bool { s, _ := p.Snapshot(context.Background()); return s.Completed == 1 })
}

func TestHTTPSRequestsShareOneLeaseUntilTunnelCloses(t *testing.T) {
	p, client, _ := proxyFixture(t)
	var calls atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, "secure") }))
	defer target.Close()
	for range 2 {
		resp, err := client.Get(target.URL)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || string(body) != "secure" {
			t.Fatal(string(body), err)
		}
	}
	s, _ := p.Snapshot(context.Background())
	if s.Requests != 1 || s.States["Busy"] != 1 || calls.Load() != 2 {
		t.Fatalf("HTTPS tunnel should hold one lease: %+v", s)
	}
	client.CloseIdleConnections()
	eventually(t, func() bool { s, _ := p.Snapshot(context.Background()); return s.Completed == 1 })
}

func TestConnectPreservesPipelinedBytes(t *testing.T) {
	_, _, proxyServer := proxyFixture(t)
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		conn, err := target.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		data := make([]byte, 5)
		io.ReadFull(conn, data)
		conn.Write(data)
	}()
	address := strings.TrimPrefix(proxyServer.URL, "http://")
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\nhello", target.Addr(), target.Addr())
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: "CONNECT"})
	if err != nil || resp.StatusCode != 200 {
		t.Fatal(resp, err)
	}
	data := make([]byte, 5)
	if _, err := io.ReadFull(reader, data); err != nil || string(data) != "hello" {
		t.Fatal(string(data), err)
	}
}

func TestProxyDoesNotFollowRedirects(t *testing.T) {
	_, client, _ := proxyFixture(t)
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Redirect(w, r, "/other", 302) }))
	defer target.Close()
	resp, err := client.Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 302 || calls.Load() != 1 {
		t.Fatal("proxy followed the redirect")
	}
}

func TestCheckIPRejectsInvalidResponses(t *testing.T) {
	for _, tc := range []struct {
		body  string
		code  int
		valid bool
	}{{"8.8.8.8\n", 200, true}, {"127.0.0.1", 200, false}, {"10.0.0.1", 200, false}, {"::1", 200, false}, {"<html>error</html>", 200, false}, {strings.Repeat("8", 1000), 200, false}, {"8.8.8.8", 503, false}} {
		t.Run(fmt.Sprintf("%d-%s", tc.code, tc.body[:min(12, len(tc.body))]), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.code); fmt.Fprint(w, tc.body) }))
			defer server.Close()
			ip, err := checkIP(context.Background(), server.URL, (&net.Dialer{}).DialContext, time.Second)
			if (err == nil) != tc.valid {
				t.Fatal(ip, err)
			}
		})
	}
}

func TestGuardedConnectionClosesOnCircuitLoss(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	conn := guardConnection(ctx, a, time.Second)
	defer conn.Close()
	cancel()
	b.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := b.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("connection not closed on lost circuit: %v", err)
	}
}

func TestUpstreamFailureReportsGatewayError(t *testing.T) {
	for _, tc := range []struct {
		problem error
		code    int
	}{{context.DeadlineExceeded, 504}, {errors.New("connection refused"), 502}} {
		t.Run(fmt.Sprint(tc.code), func(t *testing.T) {
			p, client, _ := proxyFixture(t)
			var calls atomic.Int32
			p.slots[0].ep.dial = func(context.Context, string, string) (net.Conn, error) { calls.Add(1); return nil, tc.problem }
			resp, err := client.Get("http://destination.invalid/")
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tc.code || calls.Load() != 1 {
				t.Fatal(resp.StatusCode, calls.Load())
			}
			eventually(t, func() bool { s, _ := p.Snapshot(context.Background()); return s.Failed == 1 && s.UsedIPs == 1 })
		})
	}
}

func TestFailoverToNextReadyInstance(t *testing.T) {
	p, client, _ := proxyFixture(t)
	if ok, err := p.publish(context.Background(), testEndpoint(t, 2, "9.9.9.9")); !ok || err != nil {
		t.Fatal(ok, err)
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
	defer target.Close()
	p.slots[0].ep.dial = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("connection refused") }
	p.slots[1].ep.dial = (&net.Dialer{}).DialContext
	resp, err := client.Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || string(body) != "ok" {
		t.Fatalf("failover did not serve from the healthy instance: %s %v %v", body, resp.StatusCode, err)
	}
	eventually(t, func() bool {
		s, _ := p.Snapshot(context.Background())
		return s.Failed == 1 && s.Completed == 1 && s.UsedIPs == 2
	})
}

func TestConnectionHeaderTokensAreRemoved(t *testing.T) {
	h := http.Header{"Connection": {"X-One, keep-alive", "X-Two"}, "X-One": {"secret"}, "X-Two": {"secret"}, "X-Keep": {"value"}, "Proxy-Authorization": {"secret"}}
	removeHopHeaders(h)
	if len(h) != 1 || h.Get("X-Keep") != "value" {
		t.Fatal(h)
	}
}

func TestClientCancellationReleasesBusyInstance(t *testing.T) {
	p, client, _ := proxyFixture(t)
	started := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }))
	defer target.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", target.URL, nil)
	result := make(chan error, 1)
	go func() {
		resp, err := client.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not reach upstream")
	}
	cancel()
	if err := <-result; err == nil {
		t.Fatal("expected client cancellation")
	}
	eventually(t, func() bool { s, _ := p.Snapshot(context.Background()); return s.States["Busy"] == 0 && s.Failed == 1 })
}
