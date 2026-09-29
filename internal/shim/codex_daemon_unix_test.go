//go:build unix

package shim

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/keystore"
)

func TestCodexEmbeddedRoutingCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name string
		help string
		want bool
	}{
		{"supported", "printf 'Options:\\n      --no-daemon\\n          Run without the shared server\\n'", true},
		// A slow stable-version response should use the matrix, without a help probe.
		{"slow version", "exit 9", true},
		{"older", "printf 'Options:\\n      --no-alt-screen\\n'", false},
		{"failed", "exit 1", false},
		{"hung", "exec /bin/sleep 10", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sandbox(t)
			repo := boundRepo(t, testProjectID)
			if err := SaveRecord(Record{ProjectID: testProjectID, Endpoint: testEndpoint, Signals: []string{"logs"}, Harnesses: []string{AgentCodex}, CLI: true}); err != nil {
				t.Fatal(err)
			}
			if err := keystore.SetFor(AgentCodex, testProjectID, testKey, keystore.Hosts{}); err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			body := "#!/bin/sh\n[ \"$#\" = 1 ] && [ \"$1\" = --help ] || exit 9\n" + tc.help + "\n"
			if tc.name == "slow version" {
				body = "#!/bin/sh\n[ \"$1\" = --version ] || exit 9\n/bin/sleep 0.7\nprintf 'codex-cli 0.156.0\\n'\n"
			}
			if err := os.WriteFile(filepath.Join(bin, AgentCodex), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			start := time.Now()
			r := routeFor(AgentCodex, repo, nil)
			// A generous bound checks cancellation without making scheduler jitter
			// another failure; the fake hung executable would otherwise take 10s.
			if tc.name == "hung" && time.Since(start) > 3*time.Second {
				t.Fatal("hung capability probe was not cancelled promptly")
			}
			if got := slices.Contains(r.args, "--no-daemon"); got != tc.want {
				// Under contention the deliberately delayed version can exhaust
				// the production budget. Omitting the optional flag is then the
				// correct fallback; the telemetry assertions below still apply.
				// A premature timeout (such as the old 500 ms limit) still fails.
				if tc.name != "slow version" || got || time.Since(start) < 1500*time.Millisecond {
					t.Fatal("incorrect embedded mode selection")
				}
				t.Log("version detection exhausted its budget; telemetry must survive")
			}
			if !strings.Contains(strings.Join(r.args, " "), testEndpoint) || r.env[CodexRoutedEnv] != "1" {
				t.Fatal("native telemetry routing lost")
			}
			if r := routeFor(AgentCodex, t.TempDir(), nil); len(r.args) != 0 {
				t.Fatal("unbound repository received overrides")
			}
		})
	}
}
