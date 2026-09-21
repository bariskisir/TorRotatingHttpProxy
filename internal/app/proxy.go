package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Refresh one shared deadline on traffic in either direction. A download keeps
// the tunnel alive even when the upload direction is quiet (and vice versa).
type idleConn struct {
	net.Conn
	timeout    time.Duration
	mu         sync.Mutex
	readLimit  time.Time
	writeLimit time.Time
}

func earlier(a, b time.Time) time.Time {
	if !b.IsZero() && b.Before(a) {
		return b
	}
	return a
}

func (c *idleConn) touch() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	deadline := time.Now().Add(c.timeout)
	if err := c.Conn.SetReadDeadline(earlier(deadline, c.readLimit)); err != nil {
		return err
	}
	return c.Conn.SetWriteDeadline(earlier(deadline, c.writeLimit))
}

func (c *idleConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readLimit, c.writeLimit = t, t
	return c.Conn.SetDeadline(t)
}
func (c *idleConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readLimit = t
	return c.Conn.SetReadDeadline(t)
}
func (c *idleConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeLimit = t
	return c.Conn.SetWriteDeadline(t)
}

func (c *idleConn) Read(b []byte) (int, error) {
	if err := c.touch(); err != nil {
		return 0, err
	}
	return c.Conn.Read(b)
}
func (c *idleConn) Write(b []byte) (int, error) {
	if err := c.touch(); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}

type guardedConn struct {
	net.Conn
	stop func() bool
}

func (c *guardedConn) Close() error { c.stop(); return c.Conn.Close() }

func guardConnection(ctx context.Context, conn net.Conn, idle time.Duration) net.Conn {
	return &guardedConn{Conn: &idleConn{Conn: conn, timeout: idle}, stop: context.AfterFunc(ctx, func() { conn.Close() })}
}

type ProxyHandler struct {
	Pool   *Pool
	Config Config
}

func (p *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		if err := validateConnect(r.Host); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	} else if r.URL.Scheme != "http" || r.URL.Hostname() == "" || r.URL.User != nil || r.URL.Fragment != "" {
		http.Error(w, "Use an absolute http:// URL, or CONNECT for HTTPS.", http.StatusBadRequest)
		return
	}
	if strings.HasSuffix(strings.ToLower(r.URL.Hostname()), ".onion") {
		http.Error(w, "Onion destinations do not have a public exit IP and are not supported.", http.StatusBadRequest)
		return
	}
	if r.Header.Get("Upgrade") != "" {
		http.Error(w, "HTTP protocol upgrades are not supported; use CONNECT for HTTPS.", http.StatusNotImplemented)
		return
	}
	if r.Method == http.MethodConnect {
		p.serveConnect(w, r)
		return
	}
	p.serveForward(w, r)
}

// acquireError reports why no instance could serve. lastErr is the most recent
// upstream failure when other instances were already tried for this request.
func (p *ProxyHandler) acquireError(w http.ResponseWriter, err, lastErr error) {
	if !errors.Is(err, ErrNoReady) {
		http.Error(w, "IP reservation could not be persisted.", http.StatusServiceUnavailable)
		return
	}
	if lastErr != nil {
		// Every other instance already failed for this request.
		proxyError(w, lastErr)
		return
	}
	w.Header().Set("Retry-After", "10")
	http.Error(w, "No eligible verified IP is ready. Try again later.", http.StatusServiceUnavailable)
}

// serveForward tries ready instances in turn. A pre-response upstream failure
// fails over to the next instance; only when every instance fails (or none is
// ready) does the client see an error.
func (p *ProxyHandler) serveForward(w http.ResponseWriter, r *http.Request) {
	hasBody := r.ContentLength != 0
	var lastErr error
	for attempt := 0; attempt < p.Pool.Size(); attempt++ {
		if lastErr != nil && r.Context().Err() != nil {
			// The client went away; do not burn more instances.
			return
		}
		lease, err := p.Pool.Acquire(r.Context(), p.Config.ConnectTimeout)
		if err != nil {
			p.acquireError(w, err, lastErr)
			return
		}
		responded, dialed, err := p.forwardAttempt(w, r, lease)
		lease.Finish(err)
		if err == nil {
			return
		}
		lastErr = err
		if responded {
			// Response headers already reached the client; retrying would
			// corrupt the stream.
			panic(http.ErrAbortHandler)
		}
		if hasBody && dialed {
			// The body may already have reached the target; replaying it
			// could execute a state-changing request twice.
			proxyError(w, err)
			return
		}
		// Otherwise fail over to the next ready instance.
	}
	if lastErr == nil {
		http.Error(w, "No eligible verified IP is ready. Try again later.", http.StatusServiceUnavailable)
		return
	}
	proxyError(w, lastErr)
}

func validateConnect(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return errors.New("CONNECT requires host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("invalid CONNECT port")
	}
	if strings.HasSuffix(strings.ToLower(host), ".onion") {
		return errors.New("onion destinations are not supported")
	}
	if strings.ContainsAny(host, "/\\@ \t\r\n") {
		return errors.New("invalid CONNECT host")
	}
	return nil
}

func proxyError(w http.ResponseWriter, err error) {
	code := http.StatusBadGateway
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		code = http.StatusGatewayTimeout
	}
	http.Error(w, http.StatusText(code)+": Tor could not complete the upstream connection.", code)
}

// forwardAttempt proxies one request through a single lease. It reports whether
// the upstream response headers reached the client (responded) and whether the
// upstream connection was established (dialed), so the caller can decide
// between aborting, failing over, or reporting the error. It never finishes
// the lease; the caller owns that.
func (p *ProxyHandler) forwardAttempt(w http.ResponseWriter, r *http.Request, lease *Lease) (responded, dialed bool, err error) {
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := lease.Dial(ctx, network, address)
		if err == nil {
			dialed = true
		}
		return conn, err
	}
	transport := &http.Transport{
		DialContext: dial, DisableKeepAlives: true, DisableCompression: true,
		ResponseHeaderTimeout: p.Config.ConnectTimeout, ExpectContinueTimeout: time.Second,
		MaxResponseHeaderBytes: 1024 * 1024,
	}
	defer transport.CloseIdleConnections()
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Header = r.Header.Clone()
	removeHopHeaders(out.Header)
	out.Close = true
	// A fresh transport and disabled keep-alives prevent retries on a reused
	// connection and prevent IP reuse between requests.
	resp, err := transport.RoundTrip(out)
	if err != nil {
		return false, dialed, err
	}
	defer resp.Body.Close()
	removeHopHeaders(resp.Header)
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	if len(resp.Trailer) > 0 {
		for key := range resp.Trailer {
			w.Header().Add("Trailer", key)
		}
	}
	w.WriteHeader(resp.StatusCode)
	buffer := buffers.Get().([]byte)
	defer buffers.Put(buffer)
	_, err = io.CopyBuffer(flushWriter{w}, resp.Body, buffer)
	if err != nil {
		// Headers were already sent: the caller must abort instead of
		// returning a truncated success response with a normal terminator.
		return true, dialed, err
	}
	for key, values := range resp.Trailer {
		w.Header()[key] = values
	}
	return true, dialed, nil
}

type flushWriter struct{ http.ResponseWriter }

func (w flushWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if err == nil {
		err = http.NewResponseController(w.ResponseWriter).Flush()
	}
	return n, err
}

func removeHopHeaders(h http.Header) {
	for _, connection := range h.Values("Connection") {
		for _, name := range strings.Split(connection, ",") {
			h.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(name)
	}
}

var buffers = sync.Pool{New: func() any { return make([]byte, 32*1024) }}

// serveConnect establishes a CONNECT tunnel. A dial failure fails over to the
// next ready instance; only when every instance fails does the client see an
// error. Once the tunnel is established its lifetime owns the lease.
func (p *ProxyHandler) serveConnect(w http.ResponseWriter, r *http.Request) {
	var lastErr error
	for attempt := 0; attempt < p.Pool.Size(); attempt++ {
		if lastErr != nil && r.Context().Err() != nil {
			// The client went away; do not burn more instances.
			return
		}
		lease, err := p.Pool.Acquire(r.Context(), p.Config.ConnectTimeout)
		if err != nil {
			p.acquireError(w, err, lastErr)
			return
		}
		tunneled, err := p.connectAttempt(w, r, lease)
		lease.Finish(err)
		if !tunneled {
			// Dial failed before anything was sent; try the next instance.
			lastErr = err
			continue
		}
		return
	}
	if lastErr == nil {
		http.Error(w, "No eligible verified IP is ready. Try again later.", http.StatusServiceUnavailable)
		return
	}
	proxyError(w, lastErr)
}

func (p *ProxyHandler) connectAttempt(w http.ResponseWriter, r *http.Request, lease *Lease) (bool, error) {
	upstream, err := lease.Dial(r.Context(), "tcp", r.Host)
	if err != nil {
		return false, err
	}
	defer upstream.Close()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "CONNECT requires HTTP/1.1", http.StatusHTTPVersionNotSupported)
		return true, errors.New("hijacking unavailable")
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		return true, err
	}
	defer client.Close()
	// net/http can leave a header-reading deadline on a hijacked connection.
	_ = client.SetDeadline(time.Time{})
	client = &idleConn{Conn: client, timeout: p.Config.IdleTimeout}
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return true, err
	}
	return true, relayTunnel(client, buffered.Reader, upstream)
}

func relayTunnel(client net.Conn, clientReader *bufio.Reader, upstream net.Conn) error {
	result := make(chan error, 2)
	copyDirection := func(dst net.Conn, src io.Reader) {
		buffer := buffers.Get().([]byte)
		defer buffers.Put(buffer)
		_, err := io.CopyBuffer(dst, src, buffer)
		result <- err
	}
	// Reader preserves TLS bytes pipelined after the CONNECT headers.
	go copyDirection(upstream, clientReader)
	go copyDirection(client, upstream)
	err := <-result
	client.Close()
	upstream.Close()
	<-result
	return err
}

// Track accepted sockets, including hijacked CONNECT sockets, so shutdown can
// close every connection. Overload never creates an unbounded goroutine queue.
type boundedListener struct {
	net.Listener
	mu          sync.Mutex
	connections map[*trackedConn]struct{}
	limit       int
	idle        time.Duration
}

type trackedConn struct {
	net.Conn
	owner *boundedListener
	once  sync.Once
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.owner.mu.Lock(); delete(c.owner.connections, c); c.owner.mu.Unlock() })
	return err
}

func newBoundedListener(l net.Listener, limit int, idle time.Duration) *boundedListener {
	return &boundedListener{Listener: l, connections: make(map[*trackedConn]struct{}), limit: limit, idle: idle}
}

func (l *boundedListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		l.mu.Lock()
		if len(l.connections) < l.limit {
			c := &trackedConn{Conn: &idleConn{Conn: conn, timeout: l.idle}, owner: l}
			l.connections[c] = struct{}{}
			l.mu.Unlock()
			return c, nil
		}
		l.mu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
		_, _ = fmt.Fprint(conn, "HTTP/1.1 503 Service Unavailable\r\nConnection: close\r\nContent-Length: 0\r\nRetry-After: 10\r\n\r\n")
		conn.Close()
	}
}

func (l *boundedListener) closeConnections() {
	l.mu.Lock()
	connections := make([]*trackedConn, 0, len(l.connections))
	for c := range l.connections {
		connections = append(connections, c)
	}
	l.mu.Unlock()
	for _, c := range connections {
		c.Close()
	}
}
