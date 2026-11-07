// Package sensorctl runs the sensor and talks to a running one: `terma sensor` is built
// on it.
package sensorctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/sensor"
	"github.com/miradorlabs/terma-cli/internal/sensor/platform"
)

// Options configure a running sensor.
type Options struct {
	Socket string
	// Roots are the process names that start a session: the agents' own executables.
	Roots      []string
	PolicyFile string
	// Docker is the container engine's socket; empty skips containers.
	Docker string
	// Relay is the relay's base URL the sensor sends its periodic rollups to, with the
	// relay's token; empty keeps them local.
	Relay, RelayToken string
	Log               *slog.Logger
}

// DefaultSocket is where the query API listens on this platform.
func DefaultSocket() string { return platform.DefaultSocket() }

// Run runs the sensor until ctx ends.
func Run(ctx context.Context, o Options) error {
	b, err := platform.New(o.Log)
	if err != nil {
		return err
	}
	defer b.Close()
	st := sensor.NewState(b, o.Roots, o.Log)
	for _, p := range b.Running(o.Roots) {
		if err := st.Adopt(p); err != nil {
			o.Log.Warn("adopt", "pid", p.PID, "err", err)
		}
	}
	if o.PolicyFile != "" {
		data, err := os.ReadFile(o.PolicyFile)
		if err != nil {
			return err
		}
		var p sensor.Policy
		if err := json.Unmarshal(data, &p); err != nil {
			return err
		}
		if err := st.SetPolicy(ctx, p); err != nil {
			o.Log.Warn("policy", "err", err)
		}
	}
	go func() {
		if err := st.Serve(ctx, o.Socket); err != nil {
			o.Log.Error("serve", "err", err)
		}
	}()
	b.StartExtras(ctx, st, o.Docker, o.Log)
	if o.Relay != "" {
		go sensor.Report(ctx, st, o.Relay, o.RelayToken, o.Log)
	}
	o.Log.Info("sensor running", "socket", o.Socket, "roots", o.Roots)
	return b.Run(ctx, st)
}

func client(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
}

// Get copies the answer to a query API path to w.
func Get(sock, path string, w io.Writer) error {
	resp, err := client(sock).Get("http://sensor" + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// Send makes a request that changes the sensor: a watch, a policy.
func Send(sock, method, path string, body io.Reader) error {
	req, err := http.NewRequest(method, "http://sensor"+path, body)
	if err != nil {
		return err
	}
	resp, err := client(sock).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}
