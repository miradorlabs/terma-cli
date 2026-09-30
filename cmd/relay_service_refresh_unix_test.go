//go:build unix

package cmd

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// No real service manager is called: the existing definition is in a private HOME.
// Exercise both service preference branches with a real relay process.
func TestRefreshRestartsRelayWithStaleServiceDefinition(t *testing.T) {
	for _, service := range []string{"0", "1"} {
		t.Run(service, func(t *testing.T) {
			bin := termaBinary(t)
			dir := relaySandbox(t)
			t.Setenv("TERMA_RELAY_SERVICE", service)
			addr := freeAddr(t)
			if out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", addr, "--harness", "codex"); err != nil {
				t.Fatalf("setup: %v\n%s", err, out)
			}
			name, err := relayServiceName()
			if err != nil {
				t.Fatal(err)
			}
			path, err := relayServicePath(name)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := config.WriteFileAtomic(path, []byte("stale service definition"), 0o600); err != nil {
				t.Fatal(err)
			}
			shim := plantLegacyShim(t, "codex")
			if err := routing.SaveRecord(routing.Record{ProjectID: "team", Signals: []string{"logs"}, Harnesses: []string{"codex"}}); err != nil {
				t.Fatal(err)
			}
			if _, running, err := relayStats(dir); err != nil || running {
				t.Fatalf("relay unexpectedly alive before refresh: %v %v", running, err)
			}
			t.Cleanup(func() { stopRelay(dir) })
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			proc := exec.CommandContext(ctx, bin, "update", "--refresh")
			proc.Env = os.Environ()
			if out, err := proc.CombinedOutput(); err != nil {
				t.Fatalf("refresh: %v\n%s", err, out)
			}
			if _, running, err := relayStats(dir); err != nil || !running {
				t.Fatalf("stale service definition left no listener: %v %v", running, err)
			}
			if _, err := os.Stat(shim); !os.IsNotExist(err) {
				t.Fatalf("working replacement did not remove shim: %v", err)
			}
		})
	}
}

func TestRefreshKeepsShimWhenRelayCannotListen(t *testing.T) {
	bin := termaBinary(t)
	dir := relaySandbox(t)
	t.Setenv("TERMA_RELAY_SERVICE", "0")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", ln.Addr().String(), "--harness", "codex"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	shim := plantLegacyShim(t, "codex")
	if err := routing.SaveRecord(routing.Record{ProjectID: "team", Signals: []string{"logs"}, Harnesses: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopRelay(dir) })
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	proc := exec.CommandContext(ctx, bin, "update", "--refresh")
	proc.Env = os.Environ()
	if out, err := proc.CombinedOutput(); err == nil {
		t.Fatalf("refresh succeeded without a replacement relay:\n%s", out)
	}
	if _, err := os.Stat(shim); err != nil {
		t.Fatalf("failed replacement removed shim %s: %v", filepath.Base(shim), err)
	}
}
