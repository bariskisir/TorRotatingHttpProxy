package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// Opt-in: an already running container and Internet/Tor access are required.
// Normal go test runs skip this test and never contact external services.
func TestLiveHTTPAndHTTPS(t *testing.T) {
	proxyAddress, dashboard := os.Getenv("TORPROXY_URL"), os.Getenv("TORPROXY_DASHBOARD")
	if proxyAddress == "" || dashboard == "" {
		t.Skip("set TORPROXY_URL and TORPROXY_DASHBOARD to run live Tor smoke tests")
	}
	proxyURL, err := url.Parse(proxyAddress)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 40 * time.Second}
	api := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	seen := make(map[string]bool)
	for _, destination := range []string{"http://api.ipify.org", "https://api.ipify.org"} {
		var snapshot struct {
			Unique    bool `json:"unique_ip"`
			Instances []struct {
				State string `json:"state"`
				IP    string `json:"ip"`
			} `json:"instances"`
		}
		for {
			req, _ := http.NewRequestWithContext(ctx, "GET", dashboard+"/api/status", nil)
			resp, err := api.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			err = json.NewDecoder(resp.Body).Decode(&snapshot)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			ready := false
			for _, instance := range snapshot.Instances {
				if instance.State == "Ready" {
					ready = true
				}
			}
			if ready {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("no ready Tor exit before deadline")
			case <-time.After(time.Second):
			}
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", destination, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: status %d, %s, %v", destination, resp.StatusCode, body, err)
		}
		ip, err := netip.ParseAddr(strings.TrimSpace(string(body)))
		if err != nil || !ip.Is4() {
			t.Fatalf("invalid exit IP: %s", body)
		}
		matched := false
		for _, instance := range snapshot.Instances {
			if instance.State == "Ready" && instance.IP == ip.String() {
				matched = true
			}
		}
		if !matched {
			t.Fatalf("observed exit %s was not a verified ready IP before the request", ip)
		}
		if snapshot.Unique && seen[ip.String()] {
			t.Fatalf("IP reused in unique mode: %s", ip)
		}
		seen[ip.String()] = true
		resp, err = api.Get(dashboard + "/api/ips?used=true&search=" + url.QueryEscape(ip.String()))
		if err != nil {
			t.Fatal(err)
		}
		var history struct {
			Items []struct {
				IP   string `json:"ip"`
				Used bool   `json:"used"`
			} `json:"items"`
		}
		err = json.NewDecoder(resp.Body).Decode(&history)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, record := range history.Items {
			if record.IP == ip.String() && record.Used {
				found = true
			}
		}
		if !found {
			t.Fatal("exit IP was not persisted as used")
		}
		t.Logf("%s -> %s, HTTP 200, verified circuit and persistent used flag", destination, ip)
	}
}
