package app

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestControlMultilineAndInterleavedEvents(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	c := newControl(client)
	defer c.Close()
	go func() {
		_, _ = bufio.NewReader(server).ReadString('\n')
		fmt.Fprint(server, "250+circuit-status=\r\n7 BUILT $relay PURPOSE=GENERAL\r\n..escaped\r\n.\r\n650 CIRC 8 CLOSED\r\n250 OK\r\n")
	}()
	lines, err := c.command(context.Background(), "GETINFO circuit-status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "\n.escaped") {
		t.Fatal(lines)
	}
	select {
	case event := <-c.events:
		if event != "CIRC 8 CLOSED" {
			t.Fatal(event)
		}
	case <-time.After(time.Second):
		t.Fatal("event lost")
	}
}

func TestControlTimeoutClosesDesynchronizedConnection(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	c := newControl(client)
	defer c.Close()
	go func() { _, _ = bufio.NewReader(server).ReadString('\n') }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.command(ctx, "GETINFO version"); err == nil {
		t.Fatal("expected deadline error")
	}
	select {
	case <-c.done:
	default:
		t.Fatal("connection remained usable after missing a reply")
	}
}

func TestStreamAttachmentRejectsStaleGenerationAndDetach(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	c := newControl(client)
	defer c.Close()
	ep := testEndpoint(t, 1, "8.8.8.8")
	s := &torSession{ctrl: c}
	s.setEndpoint(ep)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.events(ctx)
	reader := bufio.NewReader(server)
	// These events must not produce controller commands or prevent bootstrap.
	for _, event := range []string{
		"STREAM 9 NEW 0 131.188.40.189:443 PURPOSE=DIR_FETCH",
		"STREAM 9 DETACHED 0 131.188.40.189:443 REASON=TIMEOUT",
		"STREAM 9 CLOSED 0 131.188.40.189:443 REASON=DONE",
	} {
		fmt.Fprintf(server, "650 %s\r\n", event)
	}
	for _, test := range []struct{ event, command string }{
		{`STREAM 12 NEW 0 example.com:80 SOCKS_USERNAME="abcd" SOCKS_PASSWORD="generation"`, "ATTACHSTREAM 12 7"},
		{`STREAM 13 NEW 0 example.com:80 SOCKS_USERNAME="dead"`, "CLOSESTREAM 13 1"},
		{`STREAM 14 NEW 0 example.com:80`, "CLOSESTREAM 14 1"},
		{`STREAM 12 DETACHED 7 example.com:80 REASON=EXITPOLICY`, "CLOSESTREAM 12 1"},
	} {
		server.SetDeadline(time.Now().Add(time.Second))
		if _, err := fmt.Fprintf(server, "650 %s\r\n", test.event); err != nil {
			t.Fatal(err)
		}
		line, err := reader.ReadString('\n')
		if err != nil || strings.TrimSpace(line) != test.command {
			t.Fatalf("got %q (%v), want %q", line, err, test.command)
		}
		fmt.Fprint(server, "250 OK\r\n")
	}
	fmt.Fprint(server, "650 CIRC 7 CLOSED REASON=FINISHED\r\n")
	select {
	case <-ep.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("closed circuit was not invalidated")
	}
}
