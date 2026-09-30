package cmd

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func relaySandbox(t *testing.T) string {
	t.Helper()
	sandboxMachine(t)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	dir, err := claim.Dir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// setup points both agents at the relay with the local token, and the connect journal
// it leaves is what `terma telemetry disconnect` reverts.
func TestRelaySetupPointsAgentsAtTheRelay(t *testing.T) {
	relaySandbox(t)
	addr := freeAddr(t)
	out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", addr)
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	token, err := relayToken()
	if err != nil || len(token) < 32 {
		t.Fatalf("token = %q, %v", token, err)
	}
	claude, _ := os.ReadFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"))
	codex, _ := os.ReadFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"))
	for name, data := range map[string][]byte{"claude": claude, "codex": codex} {
		if !strings.Contains(string(data), "http://"+addr) || !strings.Contains(string(data), token) {
			t.Errorf("%s does not export to the relay with its token:\n%s", name, data)
		}
	}
	if !claim.Enabled() {
		t.Fatal("hooks do not see the relay as set up")
	}
	// Idempotent: the token is kept, so the agents' configuration does not churn.
	if _, err := runTerma(t, "relay", "setup", "--no-start", "--addr", addr); err != nil {
		t.Fatal(err)
	}
	if again, _ := relayToken(); again != token {
		t.Fatal("a second setup minted a new token")
	}
	for _, h := range []string{"claude", "codex"} {
		if out, err := runTerma(t, "telemetry", "disconnect", h, "--yes"); err != nil {
			t.Fatalf("disconnect %s: %v\n%s", h, err, out)
		}
	}
	claude, _ = os.ReadFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"))
	if strings.Contains(string(claude), addr) {
		t.Fatalf("disconnect left Claude on the relay:\n%s", claude)
	}
}

// Without setup there is no token, and the relay refuses to run rather than accept
// telemetry from anyone.
func TestRelayRunNeedsSetup(t *testing.T) {
	relaySandbox(t)
	out, err := within(5*time.Second).combined(t, "relay", "run", "--addr", freeAddr(t))
	if err == nil || !strings.Contains(err.Error(), "`terma relay setup`") {
		t.Fatalf("relay ran without setup: %v\n%s", err, out)
	}
}

// Two relays never run at once: hooks that race to start one leave exactly one. A
// relay holds relay.lock for its life; one that finds it held exits at once, happily.
func TestRelayRunsOnce(t *testing.T) {
	dir := relaySandbox(t)
	addr := freeAddr(t)
	if out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", addr, "--harness", "codex"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	unlock, err := flock.TryLock(filepath.Join(dir, relayLockFile))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	out, err := within(2*time.Second).combined(t, "relay", "run", "--idle", "1h")
	if err != nil || !strings.Contains(out, "already running") || squatted(addr) {
		t.Fatalf("a second relay started: %v\n%s", err, out)
	}
}

// Something else on the relay's port receives the agents' telemetry. The relay cannot
// take the port back, so it says why it did not start, and status names the squatter.
func TestRelayReportsASquatter(t *testing.T) {
	dir := relaySandbox(t)
	squatter, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = squatter.Close() }()
	addr := squatter.Addr().String()
	if out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", addr, "--harness", "codex"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if out, err := within(3*time.Second).combined(t, "relay", "run", "--quiet"); err == nil {
		t.Fatalf("relay started on a taken port:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, relayErrorFile)); err != nil {
		t.Fatal("the failure was not recorded for status")
	}
	out, err := runTerma(t, "relay", "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	for _, want := range []string{"another process is listening on " + addr, "last failed to start"} {
		if !strings.Contains(out, want) {
			t.Errorf("status should say %q:\n%s", want, out)
		}
	}
}

// doctor's export check on a relayed machine: what stops this repository's sessions
// from leaving, each named with its fix.
func TestRelayDoctorCheck(t *testing.T) {
	dir := relaySandbox(t)
	addr := freeAddr(t)
	if out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", addr); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if c := relayDoctorCheck("", nil); c.Status != doctor.Warn || !strings.Contains(c.Detail, "not bound") {
		t.Fatalf("unbound: %+v", c)
	}
	if c := relayDoctorCheck("proj_x", nil); c.Status != doctor.Warn || !strings.Contains(c.Detail, "no key") {
		t.Fatalf("no key: %+v", c)
	}
	keys := `{"keys":{"proj_x":"ter_srv_1"}}`
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "keys.json"), []byte(keys), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := relayDoctorCheck("proj_x", nil); c.Status != doctor.Pass {
		t.Fatalf("ready: %+v", c)
	}
	squatter, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if c := relayDoctorCheck("proj_x", nil); c.Status != doctor.Fail || !strings.Contains(c.Detail, "another process") {
		t.Fatalf("squatted: %+v", c)
	}
	_ = squatter.Close()
	if out, err := runTerma(t, "telemetry", "disconnect", "codex", "--yes"); err != nil {
		t.Fatalf("disconnect: %v\n%s", err, out)
	}
	if c := relayDoctorCheck("proj_x", nil); c.Status != doctor.Fail || !strings.Contains(c.Detail, "Codex") || c.Fix != "terma relay setup" {
		t.Fatalf("codex pointed elsewhere: %+v", c)
	}
}

// A Codex daemon reads its exporter when it starts: one running from before `relay
// setup` still exports where it did. setup says so, and doctor warns until it restarts;
// terma never restarts it (Codex says running work may be interrupted).
func TestRelayCodexDaemonPredatesSetup(t *testing.T) {
	dir := relaySandbox(t)
	codex := os.Getenv("CODEX_HOME")
	daemonDir := filepath.Join(codex, "app-server-daemon")
	if err := os.MkdirAll(daemonDir, 0o700); err != nil {
		t.Fatal(err)
	}
	record := func(started time.Time) {
		rec := fmt.Sprintf(`{"pid":%d,"processIdentity":{"startSeconds":%d}}`, os.Getpid(), started.Unix())
		if err := os.WriteFile(filepath.Join(daemonDir, "daemon.pid"), []byte(rec), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	record(time.Now().Add(-time.Hour))
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "keys.json"), []byte(`{"keys":{"proj_x":"ter_srv_1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", freeAddr(t))
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if !strings.Contains(out, "`codex app-server daemon restart`") {
		t.Errorf("setup did not name the daemon restart:\n%s", out)
	}
	if c := relayDoctorCheck("proj_x", nil); c.Status != doctor.Warn || !strings.Contains(c.Fix, "codex app-server daemon restart") {
		t.Fatalf("a daemon from before the setup: %+v", c)
	}
	record(time.Now().Add(time.Minute)) // restarted since
	if c := relayDoctorCheck("proj_x", nil); c.Status != doctor.Pass {
		t.Fatalf("a daemon started after the setup: %+v", c)
	}
}

// status says what doctor says about a relayed machine, in one line.
func TestRelayStatusAgreesWithDoctor(t *testing.T) {
	relaySandbox(t)
	repo := installRepo(t)
	_ = repo
	addr := freeAddr(t)
	if out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", addr); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	out, _ := runTerma(t, "status")
	if !strings.Contains(out, "Agents:      local relay on "+addr) || !strings.Contains(out, "this repository is not bound") {
		t.Fatalf("status does not report the relay:\n%s", out)
	}
	if strings.Contains(out, "none connected") {
		t.Fatalf("status reads a relayed machine as unconnected:\n%s", out)
	}
}

// The service definitions: valid (plutil lints the plist on macOS), running the relay
// with no idle exit, carrying the config directory, and named per config directory so
// a sandbox never touches the real service.
func TestRelayServiceDefinitions(t *testing.T) {
	env := map[string]string{"TERMA_CONFIG_DIR": "/tmp/a & b", "TERMA_ENV": "dev"}
	plist := launchdPlist("ai.terma.relay.x", "/opt/terma/bin/terma", "/tmp/log", env)
	for _, want := range []string{"<string>relay</string><string>run</string><string>--idle</string><string>0</string>", "<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>", "/tmp/a &amp; b"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q:\n%s", want, plist)
		}
	}
	if _, err := exec.LookPath("plutil"); err == nil {
		path := filepath.Join(t.TempDir(), "p.plist")
		_ = os.WriteFile(path, []byte(plist), 0o644)
		if out, err := exec.Command("plutil", "-lint", path).CombinedOutput(); err != nil {
			t.Fatalf("plutil: %v\n%s", err, out)
		}
	}
	unit := systemdUnit("/opt/terma/bin/terma", env)
	for _, want := range []string{`ExecStart="/opt/terma/bin/terma" relay run --idle 0 --quiet`, `Environment="TERMA_CONFIG_DIR=/tmp/a & b"`, "Restart=on-failure"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	a, _ := relayServiceName()
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	b, _ := relayServiceName()
	if a == b || !strings.HasPrefix(a, "ai.terma.relay.") {
		t.Fatalf("service names %q and %q must differ per config directory", a, b)
	}
}
