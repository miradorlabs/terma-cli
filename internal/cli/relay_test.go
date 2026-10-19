package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
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
	useConfigDir(t, t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	return claim.Dir(testApp.stateDir)
}

// setup points the agents at the relay with the local token, journaled for disconnect.
func TestRelaySetupPointsAgentsAtTheRelay(t *testing.T) {
	relaySandbox(t)
	addr := freeAddr(t)
	out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", addr)
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	token, err := daemon.Token(testApp.stateDir)
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
	if !claim.Enabled(testApp.stateDir) {
		t.Fatal("hooks do not see the relay as set up")
	}
	// Idempotent: the token is kept, so the agents' configuration does not churn.
	if _, err := runTerma(t, "relay", "setup", "--no-start", "--addr", addr); err != nil {
		t.Fatal(err)
	}
	if again, _ := daemon.Token(testApp.stateDir); again != token {
		t.Fatal("a second setup minted a new token")
	}
	for _, h := range []string{"claude", "codex"} {
		disconnect(t, h)
	}
	claude, _ = os.ReadFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"))
	if strings.Contains(string(claude), addr) {
		t.Fatalf("disconnect left Claude on the relay:\n%s", claude)
	}
}

// Without setup there is no token, and the relay refuses to run.
func TestRelayRunNeedsSetup(t *testing.T) {
	relaySandbox(t)
	out, err := within(5*time.Second).combined(t, "relay", "run", "--addr", freeAddr(t))
	if err == nil || !strings.Contains(err.Error(), "`terma setup`") {
		t.Fatalf("relay ran without setup: %v\n%s", err, out)
	}
}

// Racing hooks leave exactly one relay: it holds relay.lock, and a second exits at once.
func TestRelayRunsOnce(t *testing.T) {
	dir := relaySandbox(t)
	addr := freeAddr(t)
	if out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", addr, "--harness", "codex"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	unlock, err := flock.TryLock(filepath.Join(dir, daemon.LockFile))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	out, err := within(2*time.Second).combined(t, "relay", "run", "--idle", "1h")
	if err != nil || !strings.Contains(out, "already running") || daemon.Squatted(addr) {
		t.Fatalf("a second relay started: %v\n%s", err, out)
	}
}

// A squatter on the relay's port is named by doctor while no relay serves it; the relay
// then moves to a free port, points the agents that exported to the squatter at it, and
// doctor tells of the move.
func TestARelayMovesOffASquattedPort(t *testing.T) {
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
	if c := doctor.RelayCheck(testApp.agents, testApp.relayFacts(), testApp.storedKeys, "proj_x", "", nil); c.Status != doctor.Fail || !strings.Contains(c.Detail, "another process is listening on "+addr) {
		t.Errorf("squatted relay: %+v", c)
	}
	if out, err := within(10*time.Second).combined(t, "relay", "run", "--quiet", "--idle", "1ms"); err != nil {
		t.Fatalf("relay run: %v\n%s", err, out)
	}
	moved := daemon.Addr(dir)
	if moved == addr {
		t.Fatal("the relay did not move off the squatted port")
	}
	codex, _ := os.ReadFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"))
	token, _ := daemon.Token(testApp.stateDir)
	if !strings.Contains(string(codex), "http://"+moved) || strings.Contains(string(codex), "http://"+addr) || !strings.Contains(string(codex), token) {
		t.Errorf("Codex does not export to where the relay moved, with its new token:\n%s", codex)
	}
	facts := testApp.relayFacts()
	if c := doctor.RelayCheck(testApp.agents, facts, testApp.storedKeys, "proj_x", "", []string{"codex"}); c.Status != doctor.Warn || !strings.Contains(c.Detail, "moved to "+moved) {
		t.Errorf("moved relay: %+v", c)
	}
	// An agent whose exporter no settings file shows is not re-pointed by the relay: doctor
	// sends the developer to setup until setup has pointed it.
	if c := doctor.RelayCheck(testApp.agents, facts, testApp.storedKeys, "proj_x", "", []string{"codex", "gemini"}); c.Status != doctor.Fail || c.Fix != "terma setup" || !strings.Contains(c.Detail, "Gemini") {
		t.Errorf("moved relay, Gemini selected: %+v", c)
	}
	daemon.Repointed(dir, moved)
	facts = testApp.relayFacts()
	if c := doctor.RelayCheck(testApp.agents, facts, testApp.storedKeys, "proj_x", "", []string{"codex", "gemini"}); c.Status != doctor.Warn || !strings.Contains(c.Detail, "moved to "+moved) {
		t.Errorf("moved relay, setup since: %+v", c)
	}
	facts.Move.Recent = false
	if c := doctor.RelayCheck(testApp.agents, facts, testApp.storedKeys, "proj_x", "", []string{"codex", "gemini"}); strings.Contains(c.Detail, "moved to") {
		t.Errorf("a move past its notice, setup since: %+v", c)
	}
	// An address setup chose since replaces the move.
	if err := testApp.moveRelay(freeAddr(t)); err != nil {
		t.Fatal(err)
	}
	if m := testApp.relayFacts().Move; m != nil {
		t.Errorf("doctor still tells of a move to an address setup replaced: %+v", m)
	}
	facts.Move, facts.LastFailure = nil, "listen on "+addr+": address already in use"
	if c := doctor.RelayCheck(testApp.agents, facts, testApp.storedKeys, "proj_x", "", []string{"codex"}); c.Status != doctor.Warn || !strings.Contains(c.Detail, "last failed to start") {
		t.Errorf("failed relay: %+v", c)
	}
}

// Setup points every agent it sets up at where the relay moved, those whose exporter the
// relay cannot read included, records that it has, and says agents started before the move
// still send to the old address.
func TestSetupFollowsTheRelaysMove(t *testing.T) {
	dir := relaySandbox(t)
	t.Setenv("GEMINI_CLI_HOME", t.TempDir())
	from, to := freeAddr(t), freeAddr(t)
	if out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", from, "--harness", "codex,gemini"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, daemon.AddrFile), []byte(to+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	moved := fmt.Sprintf(`{"from":%q,"to":%q,"at":%q}`, from, to, time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, daemon.MovedFile), []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}
	var warned []string
	err := testApp.connectMachineRelay(t.Context(), []string{"codex", "gemini"}, "", relayReport{
		ok: func(string, string) {}, warn: func(_, what string) { warned = append(warned, what) }, then: func(string) {}, detail: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	codex, _ := os.ReadFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"))
	gemini, _ := os.ReadFile(filepath.Join(os.Getenv("GEMINI_CLI_HOME"), ".gemini", "settings.json"))
	for name, data := range map[string][]byte{"Codex": codex, "Gemini": gemini} {
		if !strings.Contains(string(data), "http://"+to) || strings.Contains(string(data), "http://"+from) {
			t.Errorf("%s does not export to where the relay moved:\n%s", name, data)
		}
	}
	if m, ok := daemon.LastMove(dir); !ok || !m.Repointed {
		t.Errorf("setup did not record that it pointed the agents at the moved relay: %+v", m)
	}
	if !slices.ContainsFunc(warned, func(w string) bool {
		return strings.Contains(w, "moved to "+to) && strings.Contains(w, "still send to "+from)
	}) {
		t.Errorf("setup did not tell of the move: %q", warned)
	}
}

// Teardown undoes an exporter the relay could not re-point when it moved, though it holds the
// token the move replaced.
func TestTeardownFindsAnExporterByATokenAMoveReplaced(t *testing.T) {
	dir := relaySandbox(t)
	t.Setenv("GEMINI_CLI_HOME", t.TempDir())
	from := freeAddr(t)
	if out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", from, "--harness", "gemini"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	old, _ := daemon.Token(testApp.stateDir)
	if err := os.WriteFile(claim.TokenPath(testApp.stateDir), []byte("rotated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	moved := fmt.Sprintf(`{"from":%q,"to":%q,"at":%q,"replaced":[%q]}`, from, freeAddr(t), time.Now().UTC().Format(time.RFC3339), old)
	if err := os.WriteFile(filepath.Join(dir, daemon.MovedFile), []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := testApp.releaseDeselected(t.Context(), nil, relayReport{ok: func(string, string) {}, then: func(string) {}}); err != nil {
		t.Fatal(err)
	}
	if settings, _ := os.ReadFile(filepath.Join(os.Getenv("GEMINI_CLI_HOME"), ".gemini", "settings.json")); strings.Contains(string(settings), old) {
		t.Fatalf("teardown left Gemini exporting with the replaced token:\n%s", settings)
	}
}

// While another holds the exporters lock past its wait, nothing changes the exporters; with
// no relay directory yet there is no one to wait for.
func TestExportersChangeOnlyUnderTheirLock(t *testing.T) {
	dir := relaySandbox(t)
	ran := false
	if err := testApp.withExporters(t.Context(), func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("no relay directory yet: ran %v, %v", ran, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := flock.TryLock(filepath.Join(dir, daemon.ExportersLockFile))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	ran = false
	if err := testApp.withExporters(ctx, func() error { ran = true; return nil }); err == nil || ran {
		t.Fatalf("changed the exporters while another held their lock: ran %v, %v", ran, err)
	}
}

// A relay that moved chooses the exporters to re-point under the exporters lock, so one that
// setup or teardown undoes meanwhile stays undone.
func TestARelayMoveLeavesAnExporterUndoneMeanwhile(t *testing.T) {
	dir := relaySandbox(t)
	from, to := freeAddr(t), freeAddr(t)
	if out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", from, "--harness", "codex"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	unlock, err := flock.TryLock(filepath.Join(dir, daemon.ExportersLockFile)) // teardown, releasing
	if err != nil {
		t.Fatal(err)
	}
	moved := make(chan struct{})
	go func() { testApp.relayMoved(t.Context(), from, to, func(string, ...any) {}); close(moved) }()
	err = testApp.releaseDeselected(t.Context(), nil, relayReport{ok: func(string, string) {}, then: func(string) {}})
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	<-moved
	if codex, _ := os.ReadFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml")); strings.Contains(string(codex), "http://"+to) {
		t.Fatalf("the relay pointed an exporter teardown had undone at where it moved:\n%s", codex)
	}
}

// doctor's export check on a relayed machine names what stops sessions leaving, with fixes.
func TestRelayDoctorCheck(t *testing.T) {
	relaySandbox(t)
	addr := freeAddr(t)
	if out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", addr); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if c := doctor.RelayCheck(testApp.agents, testApp.relayFacts(), testApp.storedKeys, "", "", nil); c.Status != doctor.Warn || !strings.Contains(c.Detail, "no team is chosen") {
		t.Fatalf("no team: %+v", c)
	}
	if c := doctor.RelayCheck(testApp.agents, testApp.relayFacts(), testApp.storedKeys, "proj_x", "", nil); c.Status != doctor.Warn || !strings.Contains(c.Detail, "no key") {
		t.Fatalf("no key: %+v", c)
	}
	keys := `{"keys":{"proj_x":"ter_srv_1"}}`
	if err := os.WriteFile(filepath.Join(testApp.dir, "keys.json"), []byte(keys), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := doctor.RelayCheck(testApp.agents, testApp.relayFacts(), testApp.storedKeys, "proj_x", "", nil); c.Status != doctor.Pass {
		t.Fatalf("ready: %+v", c)
	}
	squatter, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if c := doctor.RelayCheck(testApp.agents, testApp.relayFacts(), testApp.storedKeys, "proj_x", "", nil); c.Status != doctor.Fail || !strings.Contains(c.Detail, "another process") {
		t.Fatalf("squatted: %+v", c)
	}
	_ = squatter.Close()
	disconnect(t, "codex")
	if c := doctor.RelayCheck(testApp.agents, testApp.relayFacts(), testApp.storedKeys, "proj_x", "", nil); c.Status != doctor.Fail || !strings.Contains(c.Detail, "Codex") || c.Fix != "terma setup" {
		t.Fatalf("codex pointed elsewhere: %+v", c)
	}
}

// An agent daemon started before `terma relay setup` still exports where it did: setup
// says so, and doctor warns until it restarts, which terma never does itself.
func TestRelayCodexDaemonPredatesSetup(t *testing.T) {
	relaySandbox(t)
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
	if err := os.WriteFile(filepath.Join(testApp.dir, "keys.json"), []byte(`{"keys":{"proj_x":"ter_srv_1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", freeAddr(t))
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if !strings.Contains(out, "`codex app-server daemon restart`") {
		t.Errorf("setup did not name the daemon restart:\n%s", out)
	}
	if c := doctor.RelayCheck(testApp.agents, testApp.relayFacts(), testApp.storedKeys, "proj_x", "", nil); c.Status != doctor.Warn || !strings.Contains(c.Fix, "codex app-server daemon restart") {
		t.Fatalf("a daemon from before the setup: %+v", c)
	}
	record(time.Now().Add(time.Minute)) // restarted since
	if c := doctor.RelayCheck(testApp.agents, testApp.relayFacts(), testApp.storedKeys, "proj_x", "", nil); c.Status != doctor.Pass {
		t.Fatalf("a daemon started after the setup: %+v", c)
	}
}

// status says what doctor says about a relayed machine, in one line.
func TestRelayStatusAgreesWithDoctor(t *testing.T) {
	relaySandbox(t)
	gitRepo(t)
	addr := freeAddr(t)
	if out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", addr); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	out, _ := runTerma(t, "status")
	if !strings.Contains(out, "Agents:      local relay on "+addr) || !strings.Contains(out, "no team is chosen") {
		t.Fatalf("status does not report the relay:\n%s", out)
	}
	if strings.Contains(out, "none connected") {
		t.Fatalf("status reads a relayed machine as unconnected:\n%s", out)
	}
}

// `terma relay run` records the environment it delivers to, which doctor compares with
// the profile's: the relay a hook starts without TERMA_ENV must be told apart. One started
// by hand is not the service's, even never idling, so setup replaces it with the service.
func TestRelayRunRecordsItsEnvironment(t *testing.T) {
	dir := relaySandbox(t)
	addr := freeAddr(t)
	if out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", addr, "--harness", "codex"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	t.Setenv("TERMA_ENV", "dev")
	done := make(chan error, 1)
	go func() {
		_, err := within(3*time.Second).combined(t, "relay", "run", "--idle", "0", "--quiet")
		done <- err
	}()
	awaitRelayRecord(t, dir, 3*time.Second, done)
	info, ok := daemon.RunningRelay(dir)
	if !ok {
		t.Fatal("a relay that recorded itself does not read as running")
	}
	if info.Environment != "dev" || info.Launch != daemon.LaunchManual {
		t.Fatalf("recorded %+v, want a manually started relay delivering to dev", info)
	}
	facts := testApp.relayFacts()
	if facts.Environment != "dev" || !facts.HookStarted {
		t.Fatalf("doctor's facts = %+v", facts)
	}
	if c := doctor.RelayCheck(testApp.agents, facts, testApp.storedKeys, "proj_x", "prod", nil); c.Status != doctor.Fail || !strings.Contains(c.Detail, "dev environment") {
		t.Fatalf("a relay in another environment passed doctor: %+v", c)
	}
	if err := <-done; err != nil {
		t.Fatalf("relay run: %v", err)
	}
}

// doctor reads the relay's record through the lock: a relay holding it that recorded
// another environment fails the export check, with the fix that replaces it.
func TestRelayDoctorFailsARelayInAnotherEnvironment(t *testing.T) {
	dir := relaySandbox(t)
	if out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", freeAddr(t)); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(testApp.dir, "keys.json"), []byte(`{"keys":{"proj_x":"ter_srv_1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	unlock, err := flock.TryLock(filepath.Join(dir, daemon.LockFile))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	record := func(env string) {
		data := fmt.Sprintf(`{"pid":%d,"environment":%q,"service":false}`, os.Getpid(), env)
		if err := os.WriteFile(filepath.Join(dir, daemon.RunFile), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	record("prod")
	c := doctor.RelayCheck(testApp.agents, testApp.relayFacts(), testApp.storedKeys, "proj_x", "dev", nil)
	if c.Status != doctor.Fail || !strings.Contains(c.Detail, "delivers to the prod environment, not this profile's dev") || c.Fix != "terma setup" {
		t.Fatalf("a production relay on a dev profile: %+v", c)
	}
	record("dev")
	if c := doctor.RelayCheck(testApp.agents, testApp.relayFacts(), testApp.storedKeys, "proj_x", "dev", nil); c.Status != doctor.Pass {
		t.Fatalf("a relay in this profile's environment: %+v", c)
	}
}

// disconnect undoes what terma wrote into an agent's own configuration, as teardown does.
func disconnect(t *testing.T, agent string) {
	t.Helper()
	if _, err := harnessOf(t, agent).Disconnect(); err != nil {
		t.Fatalf("disconnect %s: %v", agent, err)
	}
}
