package main

import (
	"cmp"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/monbooru/monbooru/internal/desktop"
)

// runHealthcheck is the body of the `monbooru healthcheck` subcommand. It
// GETs the local /health endpoint and exits 0 on a 2xx, non-zero otherwise.
func runHealthcheck(argv []string) {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	configPath := fs.String("config", "", "optional monbooru.toml to read server.bind_address from")
	timeout := fs.Duration("timeout", 3*time.Second, "probe timeout")
	if err := fs.Parse(argv); err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		os.Exit(2)
	}

	url := "http://" + resolveHealthAddr(*configPath) + "/health"
	client := &http.Client{Timeout: *timeout}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "healthcheck: %s -> %d\n", url, resp.StatusCode)
		os.Exit(1)
	}
}

// resolveHealthAddr picks the address to probe the same way the server picks
// its bind address, but WITHOUT config.Load's side effect of rewriting the
// TOML on every call (a healthcheck runs every interval).
func resolveHealthAddr(configPath string) string {
	addr := os.Getenv("MONBOORU_SERVER_BIND_ADDRESS")
	if addr == "" && configPath != "" {
		var mc struct {
			Server struct {
				BindAddress string `toml:"bind_address"`
			} `toml:"server"`
		}
		if _, err := toml.DecodeFile(configPath, &mc); err == nil {
			addr = mc.Server.BindAddress
		}
	}
	return desktop.LoopbackAddr(cmp.Or(addr, "127.0.0.1:8455"))
}
