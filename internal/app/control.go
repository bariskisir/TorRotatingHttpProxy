package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errControlClosed = errors.New("Tor control connection closed")

type controlReply struct {
	code  int
	lines []string
}

// A single reader separates asynchronous 650 events from command replies.
// Commands are serialized; losing a reply or overflowing events closes the
// connection rather than risking attachment to an unverified circuit.
type control struct {
	conn     net.Conn
	reader   *bufio.Scanner
	commands chan struct{}
	replies  chan controlReply
	events   chan string
	done     chan struct{}
	once     sync.Once
}

func newControl(conn net.Conn) *control {
	s := bufio.NewScanner(conn)
	s.Buffer(make([]byte, 4096), 1024*1024)
	c := &control{conn: conn, reader: s, commands: make(chan struct{}, 1), replies: make(chan controlReply, 1), events: make(chan string, 256), done: make(chan struct{})}
	go c.readLoop()
	return c
}

func (c *control) Close() { c.once.Do(func() { close(c.done); c.conn.Close() }) }

func (c *control) command(ctx context.Context, command string) ([]string, error) {
	if strings.ContainsAny(command, "\r\n") {
		return nil, errors.New("invalid control command")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	select {
	case c.commands <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, errControlClosed
	}
	defer func() { <-c.commands }()
	deadline, _ := ctx.Deadline()
	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		c.Close()
		return nil, err
	}
	if _, err := fmt.Fprintf(c.conn, "%s\r\n", command); err != nil {
		c.Close()
		return nil, err
	}
	select {
	case r := <-c.replies:
		if r.code != 250 {
			return nil, fmt.Errorf("Tor control %d: %s", r.code, strings.Join(r.lines, "; "))
		}
		return r.lines, nil
	case <-ctx.Done():
		c.Close()
		return nil, ctx.Err()
	case <-c.done:
		return nil, errControlClosed
	}
}

func (c *control) readLoop() {
	defer c.Close()
	var reply controlReply
	total := 0
	for c.reader.Scan() {
		line := c.reader.Text()
		if len(line) < 4 {
			return
		}
		code, err := strconv.Atoi(line[:3])
		if err != nil {
			return
		}
		separator, body := line[3], line[4:]
		if separator != ' ' && separator != '-' && separator != '+' {
			return
		}
		if separator == '+' {
			var b strings.Builder
			b.WriteString(body)
			ended := false
			for c.reader.Scan() {
				v := c.reader.Text()
				if v == "." {
					ended = true
					break
				}
				if strings.HasPrefix(v, "..") {
					v = v[1:]
				}
				if b.Len()+len(v) > 4*1024*1024 {
					return
				}
				b.WriteByte('\n')
				b.WriteString(v)
			}
			if !ended {
				return
			}
			body = b.String()
		}
		if code == 650 {
			select {
			case c.events <- body:
			default:
				return
			}
			continue
		}
		if reply.code != 0 && reply.code != code {
			return
		}
		reply.code = code
		reply.lines = append(reply.lines, body)
		total += len(body)
		if total > 4*1024*1024 {
			return
		}
		if separator == ' ' {
			select {
			case c.replies <- reply:
			case <-c.done:
				return
			}
			reply = controlReply{}
			total = 0
		}
	}
}
