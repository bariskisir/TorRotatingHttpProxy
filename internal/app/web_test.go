package app

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWebHealthReadinessPaginationAndSSE(t *testing.T) {
	store := testStore(t)
	cfg := testConfig()
	pool := NewPool(cfg, store)
	tester := NewLoadTester(context.Background(), cfg)
	server := httptest.NewServer(WebHandler(pool, store, tester))
	defer server.Close()
	client := &http.Client{Timeout: 2 * time.Second}
	for _, tc := range []struct {
		path   string
		status int
	}{{"/", 200}, {"/healthz", 200}, {"/readyz", 503}, {"/api/ips?page=0", 400}, {"/api/ips?page_size=101", 400}, {"/api/ips?used=maybe", 400}, {"/api/status", 200}} {
		resp, err := client.Get(server.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Fatalf("%s returned %d", tc.path, resp.StatusCode)
		}
	}
	if ok, err := pool.publish(context.Background(), testEndpoint(t, 1, "8.8.8.8")); !ok || err != nil {
		t.Fatal(ok, err)
	}
	resp, err := client.Get(server.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("ready instance not reflected in readiness")
	}
	for range 2 { // Each new SSE subscription must immediately get a full snapshot.
		resp, err := client.Get(server.URL + "/api/events")
		if err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(resp.Body)
		kind, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		data, err := reader.ReadString('\n')
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if kind != "event: status\n" {
			t.Fatal(kind)
		}
		var snapshot struct {
			Snapshot
			Test TestSnapshot `json:"test"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(data, "data: ")), &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.States["Ready"] != 1 || snapshot.Test.State != "idle" {
			t.Fatal(snapshot)
		}
	}
}
