package claude

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

const (
	relayEndpoint = "http://127.0.0.1:43180"
	relayToken    = "0123456789abcdef0123456789abcdef0123456789abcdef"
)

// earlierRelayInstall is what an earlier terma's relay setup wrote: the token inline, in the
// settings' env block.
func earlierRelayInstall(t *testing.T, c exporter) {
	t.Helper()
	if err := c.Connect(harness.Exporter{Endpoint: relayEndpoint, APIKey: relayToken, Signals: harness.AllSignals}, true); err != nil {
		t.Fatal(err)
	}
}

// assertRelayHelper checks the token is in the relay's headers helper alone: the settings
// name the helper and hold no token, and the helper prints the relay's Authorization header.
func assertRelayHelper(t *testing.T, c exporter, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), relayToken) {
		t.Fatalf("the relay's token is in the settings file:\n%s", raw)
	}
	if _, ok := envOf(t, path)[harness.EnvOTLPHeaders]; ok {
		t.Errorf("%s is still in the env block", harness.EnvOTLPHeaders)
	}
	helper := relayHelperPath(c.dir)
	if got := readJSON(t, path)[claudeOtelHeadersHelper]; got != helper {
		t.Fatalf("otelHeadersHelper = %v, want %q", got, helper)
	}
	info, err := os.Stat(helper)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("helper is %v, want 0700", info.Mode().Perm())
	}
	out, err := exec.Command(helper).Output()
	if err != nil {
		t.Fatalf("run helper: %v", err)
	}
	var headers map[string]string
	if err := json.Unmarshal(out, &headers); err != nil || headers["Authorization"] != "Bearer "+relayToken {
		t.Fatalf("helper printed %q (%v)", out, err)
	}
}

// assertRestored checks unconfiguring put the settings back, warning of nothing, and took
// the helper and terma's record of the install with them.
func assertRestored(t *testing.T, a Agent, c exporter, path, want string) {
	t.Helper()
	res, err := a.UnconfigureRelay(t.Context(), relayToken)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Notes) > 0 {
		t.Errorf("unconfigure noted %q", res.Notes)
	}
	if j, err := harness.LoadJournal(c.dir, c.Name(), path); err != nil || j != nil {
		t.Errorf("a record of the install outlived unconfigure: %+v, %v", j, err)
	}
	var got, original map[string]any
	if err := json.Unmarshal([]byte(want), &original); err != nil {
		t.Fatal(err)
	}
	got = readJSON(t, path)
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(original)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("settings after unconfigure = %s, want %s", gotJSON, wantJSON)
	}
	if _, err := os.Stat(relayHelperPath(c.dir)); !os.IsNotExist(err) {
		t.Errorf("the relay's helper outlived unconfigure: %v", err)
	}
}

// The relay's token reaches Claude Code through a headers helper, never the env block
// whose OTEL_* entries an older Claude Code passed to the tools it ran.
func TestConfigureRelayKeepsTokenOutOfEnv(t *testing.T) {
	if !relayHelper {
		t.Skip("the relay's token stays in the env block on Windows")
	}
	const original = `{"model":"opus"}`
	c, path := claudeIn(t, original)
	a := Agent{ConfigDir: c.dir}
	if _, err := a.ConfigureRelay(t.Context(), agents.RelayConfig{Endpoint: relayEndpoint, Token: relayToken}); err != nil {
		t.Fatal(err)
	}
	assertRelayHelper(t, c, path)
	if env := envOf(t, path); env[harness.EnvOTLPEndpoint] != relayEndpoint || env[claudeEnableTelemetry] != "1" {
		t.Errorf("env = %v", env)
	}
	if pointed, _ := a.RelayPointed("127.0.0.1:43180"); !pointed {
		t.Error("the exporter does not read as pointed at the relay")
	}
	assertRestored(t, a, c, path, original)
}

// An install from before the helper moves its token on the next setup, or on the refresh
// after an upgrade, keeping what unconfiguring restores — here a header of the developer's.
func TestEarlierInlineRelayTokenMoves(t *testing.T) {
	if !relayHelper {
		t.Skip("the relay's token stays in the env block on Windows")
	}
	const original = `{"env":{"OTEL_EXPORTER_OTLP_HEADERS":"x-team=mine"},"model":"opus"}`
	for name, move := range map[string]func(Agent) (bool, error){
		"setup": func(a Agent) (bool, error) {
			_, err := a.ConfigureRelay(t.Context(), agents.RelayConfig{Endpoint: relayEndpoint, Token: relayToken})
			return true, err
		},
		"refresh": func(a Agent) (bool, error) {
			_, changed, err := a.RefreshMachine()
			return changed, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, path := claudeIn(t, original)
			a := Agent{ConfigDir: c.dir}
			earlierRelayInstall(t, c)
			if envOf(t, path)[harness.EnvOTLPHeaders] != "Authorization=Bearer "+relayToken {
				t.Fatal("the earlier install has no inline token; the test cannot tell a move")
			}
			if changed, err := move(a); err != nil || !changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			assertRelayHelper(t, c, path)
			if _, changed, err := a.RefreshMachine(); err != nil || changed {
				t.Fatalf("a second refresh: changed=%v err=%v", changed, err)
			}
			assertRestored(t, a, c, path, original)
		})
	}
}

// A refresh moves only the relay's inline token, exactly as terma wrote it: an export
// edited since, a server key, or an env block terma has no record of is left alone.
func TestRefreshLeavesOtherExportsAlone(t *testing.T) {
	if !relayHelper {
		t.Skip("the relay's token stays in the env block on Windows")
	}
	for name, seed := range map[string]func(*testing.T, exporter, string){
		"edited since": func(t *testing.T, c exporter, path string) {
			earlierRelayInstall(t, c)
			s, err := loadSettings(path)
			if err != nil {
				t.Fatal(err)
			}
			s.merge(map[string]string{otelLogUserPrompts: "0"})
			if err := s.save(true); err != nil {
				t.Fatal(err)
			}
		},
		"server key": func(t *testing.T, c exporter, _ string) {
			e := fullExporter()
			e.Endpoint = relayEndpoint
			if err := c.Connect(e, true); err != nil {
				t.Fatal(err)
			}
		},
		"a helper of the developer's": func(t *testing.T, c exporter, path string) {
			earlierRelayInstall(t, c)
			s, err := loadSettings(path)
			if err != nil {
				t.Fatal(err)
			}
			s.root[claudeOtelHeadersHelper] = json.RawMessage(`"/usr/local/bin/my-headers"`)
			if err := s.save(true); err != nil {
				t.Fatal(err)
			}
		},
		"not the relay": func(t *testing.T, c exporter, _ string) {
			if err := c.Connect(harness.Exporter{Endpoint: "https://otel.example.com", APIKey: relayToken, Signals: harness.AllSignals}, true); err != nil {
				t.Fatal(err)
			}
		},
		"an unreadable record": func(t *testing.T, c exporter, path string) {
			earlierRelayInstall(t, c)
			if err := os.WriteFile(harness.JournalPath(c.dir, c.Name(), path), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"no record": func(t *testing.T, _ exporter, path string) {
			body := `{"env":{"CLAUDE_CODE_ENABLE_TELEMETRY":"1","OTEL_EXPORTER_OTLP_ENDPOINT":"` + relayEndpoint +
				`","OTEL_EXPORTER_OTLP_HEADERS":"Authorization=Bearer ` + relayToken + `"}}`
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, path := claudeIn(t, `{"model":"opus"}`)
			seed(t, c, path)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, changed, err := (Agent{ConfigDir: c.dir}).RefreshMachine(); err != nil || changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			if after, _ := os.ReadFile(path); string(after) != string(before) {
				t.Errorf("settings changed:\n%s", after)
			}
			if _, err := os.Stat(relayHelperPath(c.dir)); !os.IsNotExist(err) {
				t.Errorf("a relay helper was written: %v", err)
			}
		})
	}
}

// The relay listens on a loopback address, however it is spelled.
func TestLoopbackHTTP(t *testing.T) {
	t.Parallel()
	for endpoint, want := range map[string]bool{
		"http://127.0.0.1:43180": true, "http://localhost:43180": true, "http://[::1]:43180": true, "http://127.0.0.2:1": true,
		"https://127.0.0.1:43180": false, "http://otel.example.com": false, "http://10.0.0.1:43180": false, "": false,
	} {
		if got := loopbackHTTP(endpoint); got != want {
			t.Errorf("loopbackHTTP(%q) = %v, want %v", endpoint, got, want)
		}
	}
}
