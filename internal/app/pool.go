package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

var ErrNoReady = errors.New("no unused, verified IP is ready")
var errUsedIP = errors.New("IP was already used")

type InstanceView struct {
	ID        int       `json:"id"`
	State     string    `json:"state"`
	Bootstrap int       `json:"bootstrap"`
	IP        string    `json:"ip"`
	Circuit   string    `json:"circuit"`
	LastCheck time.Time `json:"last_check"`
	LastError string    `json:"last_error"`
	Requests  uint64    `json:"requests"`
}

type Event struct {
	Time     time.Time `json:"time"`
	Instance int       `json:"instance"`
	Message  string    `json:"message"`
}

type Snapshot struct {
	Started          time.Time      `json:"started"`
	Instances        []InstanceView `json:"instances"`
	States           map[string]int `json:"states"`
	Requests         uint64         `json:"requests"`
	Completed        uint64         `json:"completed"`
	Failed           uint64         `json:"failed"`
	Rejected         uint64         `json:"rejected"`
	KnownIPs         int64          `json:"known_ips"`
	UsedIPs          int64          `json:"used_ips"`
	Events           []Event        `json:"events"`
	Revision         uint64         `json:"revision"`
	UniqueIP         bool           `json:"unique_ip"`
	MaxUsedIPRetries int            `json:"max_used_ip_retries"`
}

type dialFunc func(context.Context, string, string) (net.Conn, error)

type endpoint struct {
	id      int
	ip      string
	circuit string
	token   string
	ctx     context.Context
	cancel  context.CancelFunc
	dial    dialFunc
	done    chan struct{}
	once    sync.Once
	// Last serving result, set by Lease.Finish before done is closed.
	lastErr error
	// Outstanding reuse-mode leases. Guarded by Pool.mu.
	active int
}

type slot struct {
	view InstanceView
	ep   *endpoint
	// Consecutive failed leases. Reset by the first successful lease.
	fails int
}

type Pool struct {
	mu                                              sync.Mutex
	store                                           *Store
	unique                                          bool
	maxUsedIPRetries                                int
	slots                                           []slot
	reserved                                        map[string]*endpoint
	next                                            int
	started                                         time.Time
	requests, completed, failed, rejected, revision uint64
	events                                          []Event
	// Closed and replaced whenever a slot may have become Ready, so
	// waiting Acquire calls wake up and re-scan.
	notify chan struct{}
}

func NewPool(cfg Config, store *Store) *Pool {
	p := &Pool{store: store, unique: cfg.UniqueIP, maxUsedIPRetries: cfg.MaxUsedIPRetries, slots: make([]slot, cfg.TorCount), reserved: make(map[string]*endpoint), started: time.Now(), events: []Event{}, notify: make(chan struct{})}
	for i := range p.slots {
		p.slots[i].view = InstanceView{ID: i + 1, State: "Starting"}
	}
	return p
}

func (p *Pool) eventLocked(id int, message string) {
	if len(p.events) == 100 {
		copy(p.events, p.events[1:])
		p.events = p.events[:99]
	}
	p.events = append(p.events, Event{Time: time.Now(), Instance: id, Message: message})
	p.revision++
	slog.Info(message, "instance", id)
}

func (p *Pool) state(id int, state string, progress int, problem error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := &p.slots[id-1]
	changed := s.view.State != state
	s.view.State = state
	if progress >= 0 {
		s.view.Bootstrap = progress
	}
	if problem != nil {
		s.view.LastError = problem.Error()
		changed = true
	} else if s.view.LastError != "" {
		// A clean state update clears the stale issue from the previous attempt.
		s.view.LastError = ""
		changed = true
	}
	if changed {
		message := state
		if problem != nil {
			message += ": " + problem.Error()
		}
		p.eventLocked(id, message)
	} else {
		p.revision++
	}
}

// broadcastLocked wakes waiting Acquire calls. The pool lock must be held.
func (p *Pool) broadcastLocked() {
	close(p.notify)
	p.notify = make(chan struct{})
}

// publish is the only entry into Ready. Observation and reservation are both
// performed under the pool lock so two workers cannot publish the same IP.
func (p *Pool) publish(ctx context.Context, ep *endpoint) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := &p.slots[ep.id-1]
	s.view.IP, s.view.Circuit, s.view.LastCheck = ep.ip, ep.circuit, time.Now()
	unused, err := p.store.Observe(ctx, ep.ip)
	if err != nil {
		return false, err
	}
	if p.unique && !unused {
		return false, fmt.Errorf("%w: %s", errUsedIP, ep.ip)
	}
	if (p.unique && p.reserved[ep.ip] != nil) || ep.ctx.Err() != nil {
		return false, nil
	}
	s.ep = ep
	if p.unique {
		p.reserved[ep.ip] = ep
	}
	s.view.State, s.view.LastError = "Ready", ""
	p.eventLocked(ep.id, "Ready with "+ep.ip)
	p.broadcastLocked()
	return true, nil
}

type Lease struct {
	IP       string
	Instance int
	Dial     dialFunc
	finish   func(error)
}

func (l *Lease) Finish(err error) { l.finish(err) }

func (p *Pool) Acquire(ctx context.Context, wait time.Duration) (*Lease, error) {
	deadline := time.Now().Add(wait)
	for {
		p.mu.Lock()
		lease, err := p.acquireLocked(ctx)
		notify := p.notify
		p.mu.Unlock()
		if err == nil {
			return lease, nil
		}
		if !errors.Is(err, ErrNoReady) {
			return nil, err
		}
		if remaining := time.Until(deadline); remaining <= 0 {
			p.countRejected()
			return nil, ErrNoReady
		} else {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-notify:
				// Another instance may have become Ready; re-scan.
			case <-time.After(remaining):
				// Final re-scan in case readiness landed just now.
				p.mu.Lock()
				lease, err := p.acquireLocked(ctx)
				p.mu.Unlock()
				if err == nil {
					return lease, nil
				}
				if !errors.Is(err, ErrNoReady) {
					return nil, err
				}
				p.countRejected()
				return nil, ErrNoReady
			}
		}
	}
}

func (p *Pool) countRejected() {
	p.mu.Lock()
	p.rejected++
	p.revision++
	p.mu.Unlock()
}

// acquireLocked scans for a Ready instance. The pool lock must be held.
func (p *Pool) acquireLocked(ctx context.Context) (*Lease, error) {
	if !p.unique {
		return p.acquireReuseLocked(ctx)
	}
	for offset := 0; offset < len(p.slots); offset++ {
		index := (p.next + offset) % len(p.slots)
		s := &p.slots[index]
		ep := s.ep
		if s.view.State != "Ready" || ep == nil || ep.ctx.Err() != nil {
			continue
		}
		ok, err := p.store.Consume(ctx, ep.ip, p.unique)
		if err != nil {
			return nil, fmt.Errorf("persist IP reservation: %w", err)
		}
		if !ok {
			ep.cancel()
			continue
		}
		s.view.State = "Busy"
		s.view.Requests++
		p.requests++
		p.next = (index + 1) % len(p.slots)
		p.eventLocked(ep.id, "Assigned "+ep.ip)
		return &Lease{IP: ep.ip, Instance: ep.id, Dial: ep.dial, finish: func(err error) {
			ep.once.Do(func() {
				p.mu.Lock()
				ep.lastErr = err
				if err != nil {
					p.failed++
					p.slots[ep.id-1].fails++
				} else {
					p.completed++
					p.slots[ep.id-1].fails = 0
				}
				if p.slots[ep.id-1].ep == ep {
					p.slots[ep.id-1].view.State = "Rotating"
					if err != nil {
						p.slots[ep.id-1].view.LastError = err.Error()
					}
				}
				p.eventLocked(ep.id, "Request finished; rotating")
				p.mu.Unlock()
				close(ep.done)
			})
		}}, nil
	}
	return nil, ErrNoReady
}

// acquireReuseLocked hands out concurrent leases on the same endpoint.
// The slot stays Ready while requests are in flight, so Busy never blocks
// and p.next keeps distributing the next call to the following instance.
func (p *Pool) acquireReuseLocked(ctx context.Context) (*Lease, error) {
	for offset := 0; offset < len(p.slots); offset++ {
		index := (p.next + offset) % len(p.slots)
		s := &p.slots[index]
		ep := s.ep
		if s.view.State != "Ready" || ep == nil || ep.ctx.Err() != nil {
			continue
		}
		ok, err := p.store.Consume(ctx, ep.ip, false)
		if err != nil {
			return nil, fmt.Errorf("persist IP reservation: %w", err)
		}
		if !ok {
			ep.cancel()
			continue
		}
		s.view.Requests++
		s.view.LastCheck = time.Now()
		p.requests++
		p.next = (index + 1) % len(p.slots)
		ep.active++
		p.eventLocked(ep.id, "Assigned "+ep.ip)
		var leaseOnce sync.Once
		return &Lease{IP: ep.ip, Instance: ep.id, Dial: ep.dial, finish: func(err error) {
			leaseOnce.Do(func() { p.finishReuse(ep, err) })
		}}, nil
	}
	return nil, ErrNoReady
}

// finishReuse accounts one concurrent lease. Success leaves the endpoint
// Ready for immediate reuse; failure withdraws it so the next call goes to
// another instance, then wakes the worker to rotate after in-flight drains.
func (p *Pool) finishReuse(ep *endpoint, err error) {
	p.mu.Lock()
	if ep.active > 0 {
		ep.active--
	}
	if err != nil {
		p.failed++
		p.slots[ep.id-1].fails++
		ep.lastErr = err
		if s := &p.slots[ep.id-1]; s.ep == ep {
			s.ep = nil
			s.view.State = "Rotating"
			s.view.LastError = err.Error()
			p.eventLocked(ep.id, "Request finished; rotating")
		}
		p.mu.Unlock()
		ep.once.Do(func() { close(ep.done) })
		return
	}
	p.completed++
	p.slots[ep.id-1].fails = 0
	p.mu.Unlock()
}

// reuseActive reports outstanding leases for drain waits.
func (p *Pool) reuseActive(ep *endpoint) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return ep.active
}

// Size reports the number of managed instances.
func (p *Pool) Size() int { return len(p.slots) }

// consecutiveFailures reports how many leases in a row failed on an instance.
func (p *Pool) consecutiveFailures(id int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.slots[id-1].fails
}

func (p *Pool) withdraw(ep *endpoint) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reserved[ep.ip] == ep {
		delete(p.reserved, ep.ip)
	}
	if s := &p.slots[ep.id-1]; s.ep == ep {
		s.ep = nil
		s.view.State = "Rotating"
		p.revision++
	}
	ep.cancel()
}

func (p *Pool) Ready() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.slots {
		if s.view.State == "Ready" && s.ep != nil && s.ep.ctx.Err() == nil {
			return true
		}
	}
	return false
}

func (p *Pool) Snapshot(ctx context.Context) (Snapshot, error) {
	p.mu.Lock()
	s := Snapshot{Started: p.started, Instances: make([]InstanceView, len(p.slots)), States: map[string]int{}, Requests: p.requests,
		Completed: p.completed, Failed: p.failed, Rejected: p.rejected, Events: append([]Event{}, p.events...), Revision: p.revision,
		UniqueIP: p.unique, MaxUsedIPRetries: p.maxUsedIPRetries}
	for i, slot := range p.slots {
		s.Instances[i] = slot.view
		s.States[slot.view.State]++
	}
	p.mu.Unlock()
	var err error
	s.KnownIPs, s.UsedIPs, err = p.store.Counts(ctx)
	return s, err
}
