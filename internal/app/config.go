package app

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type Config struct {
	TorCount         int
	UniqueIP         bool
	MaxUsedIPRetries int
	DataDir          string
	ProxyAddr        string
	WebAddr          string
	IPCheckURL       string
	ConnectTimeout   time.Duration
	IdleTimeout      time.Duration
	BootstrapTimeout time.Duration
	MaxConnections   int
}

func LoadConfig() (Config, error) {
	c := Config{
		DataDir: env("DATA_DIR", "/data"), ProxyAddr: env("PROXY_ADDR", ":3128"),
		WebAddr: env("WEB_ADDR", ":8080"), IPCheckURL: env("IP_CHECK_URL", "https://api.ipify.org"),
	}
	var err error
	if c.TorCount, err = positiveInt("TOR_COUNT", 10); err != nil {
		return c, err
	}
	if c.MaxUsedIPRetries, err = positiveInt("MAX_USED_IP_RETRIES", 3); err != nil {
		return c, err
	}
	switch env("UNIQUE_IP", "true") {
	case "true":
		c.UniqueIP = true
	case "false":
		c.UniqueIP = false
	default:
		return c, fmt.Errorf("UNIQUE_IP must be true or false")
	}
	if c.MaxConnections, err = positiveInt("MAX_CONNECTIONS", 1024); err != nil {
		return c, err
	}
	for _, item := range []struct {
		name     string
		target   *time.Duration
		fallback string
	}{
		{"CONNECT_TIMEOUT", &c.ConnectTimeout, "30s"}, {"IDLE_TIMEOUT", &c.IdleTimeout, "120s"},
		{"BOOTSTRAP_TIMEOUT", &c.BootstrapTimeout, "5m"},
	} {
		*item.target, err = time.ParseDuration(env(item.name, item.fallback))
		if err != nil || *item.target <= 0 {
			return c, fmt.Errorf("%s must be a positive duration, such as %s", item.name, item.fallback)
		}
	}
	u, err := url.Parse(c.IPCheckURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Fragment != "" {
		return c, fmt.Errorf("IP_CHECK_URL must be an absolute HTTP(S) URL without credentials or a fragment")
	}
	for _, address := range []string{c.ProxyAddr, c.WebAddr} {
		_, port, err := net.SplitHostPort(address)
		if err != nil {
			return c, fmt.Errorf("invalid listen address %q: %w", address, err)
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return c, fmt.Errorf("invalid listen port in %q", address)
		}
	}
	c.DataDir, err = filepath.Abs(c.DataDir)
	return c, err
}

func env(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}

func positiveInt(name string, fallback int) (int, error) {
	n, err := strconv.Atoi(env(name, strconv.Itoa(fallback)))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return n, nil
}
