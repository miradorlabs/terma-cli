package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay/service"
)

// sandboxService puts the per-user service directories under a temporary home and the
// state in a temporary directory stateDir, with the relay's dir, so a test never touches
// the machine's own relay service. TERMA_STATE_DIR names it too: the environment the
// service records is how the relay it starts finds the directory.
func sandboxService(t *testing.T) (stateDir, dir string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	stateDir = t.TempDir()
	t.Setenv("TERMA_STATE_DIR", stateDir)
	t.Setenv("TERMA_RELAY_HOLD", "")
	dir, err := Dir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	return stateDir, dir
}

// The recorded environment is the caller's relay keys, and only those.
func TestRecordEnvKeepsOnlyWhatPlacesTheRelay(t *testing.T) {
	_, dir := sandboxService(t)
	if _, ok := RecordedEnv(dir); ok {
		t.Fatal("an environment is recorded before anything recorded one")
	}
	t.Setenv("TERMA_ENV", "dev")
	t.Setenv("TERMA_API_KEY", "secret")
	if err := RecordEnv(dir); err != nil {
		t.Fatal(err)
	}
	env, ok := RecordedEnv(dir)
	// The relay a service or a hook starts resolves the same state directory as its caller.
	if !ok || env["TERMA_ENV"] != "dev" || env["TERMA_STATE_DIR"] != os.Getenv("TERMA_STATE_DIR") || env["XDG_STATE_HOME"] != os.Getenv("XDG_STATE_HOME") {
		t.Fatalf("recorded %v, %v", env, ok)
	}
	if _, leaked := env["TERMA_API_KEY"]; leaked {
		t.Fatal("RecordEnv recorded a credential")
	}
	info, err := os.Stat(filepath.Join(dir, EnvFile))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("the record is not private: mode %v", info.Mode().Perm())
	}
	if err := os.WriteFile(filepath.Join(dir, EnvFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := RecordedEnv(dir); ok {
		t.Fatal("a corrupt record reads as an environment")
	}
}

// The relay's keys come from the record exactly, a key the record lacks is unset (no
// TERMA_ENV means production, not the caller's choice), and the rest of the caller's
// environment is kept.
func TestWithRelayEnvSetsTheRecordedKeysExactly(t *testing.T) {
	t.Parallel()
	base := []string{"PATH=/usr/bin", "TERMA_ENV=staging", "TERMA_RELAY_HOLD=5s", "HOME=/home/dev", "TERMANATOR=1"}
	got := withRelayEnv(base, map[string]string{"TERMA_ENV": "dev", "HOME": "/home/dev"})
	for _, want := range []string{"PATH=/usr/bin", "TERMANATOR=1", "TERMA_ENV=dev", "HOME=/home/dev"} {
		if !slices.Contains(got, want) {
			t.Errorf("env lacks %q: %v", want, got)
		}
	}
	for _, gone := range []string{"TERMA_ENV=staging", "TERMA_RELAY_HOLD=5s"} {
		if slices.Contains(got, gone) {
			t.Errorf("env kept the caller's %q: %v", gone, got)
		}
	}
	prod := withRelayEnv([]string{"TERMA_ENV=dev"}, map[string]string{})
	if slices.ContainsFunc(prod, func(kv string) bool { return kv == "TERMA_ENV=dev" }) {
		t.Fatalf("a record without TERMA_ENV kept the caller's: %v", prod)
	}
}

// A hook-started relay runs in the recorded environment, not the hook's: the hook that
// lacks TERMA_ENV must not start a relay that delivers to production.
func TestAHookStartedRelayRunsInTheRecordedEnvironment(t *testing.T) {
	_, dir := sandboxService(t)
	t.Setenv("TERMA_ENV", "dev")
	if err := RecordEnv(dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMA_ENV", "") // the hook's environment
	proc := spawnCommand("/opt/terma/bin/terma", dir)
	if !slices.Equal(proc.Args, []string{"/opt/terma/bin/terma", "relay", "run", "--quiet"}) {
		t.Fatalf("args = %v", proc.Args)
	}
	if !slices.Contains(proc.Env, "TERMA_ENV=dev") {
		t.Fatalf("the spawned relay does not run in the recorded environment: %v", proc.Env)
	}

	// Nothing recorded yet (a machine set up by an earlier terma): the caller's, as before.
	_, fresh := sandboxService(t)
	if proc := spawnCommand("/opt/terma/bin/terma", fresh); proc.Env != nil {
		t.Fatalf("with nothing recorded the relay should inherit, got %v", proc.Env)
	}
}

// installDefinition writes the definition this terma would install in env, as if installed.
func installDefinition(t *testing.T, stateDir string, env map[string]string, edit func(string) string) string {
	t.Helper()
	m, err := serviceWith(stateDir, env)
	if err != nil {
		t.Fatal(err)
	}
	if m.Exe, err = os.Executable(); err != nil {
		t.Fatal(err)
	}
	m.Exe, _ = filepath.Abs(m.Exe)
	def, err := m.Definition()
	if err != nil {
		t.Fatal(err)
	}
	path, err := m.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(edit(def)), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func same(s string) string { return s }

// The service is checked against the recorded environment for update and doctor, and
// against the caller's for install, which records the caller's.
func TestCheckServiceComparesWithTheRightEnvironment(t *testing.T) {
	if !service.Supported() {
		t.Skip("no relay service on this platform")
	}
	stateDir, dir := sandboxService(t)
	if s := CheckService(stateDir); s.Installed || s.Current {
		t.Fatalf("no service is installed, got %+v", s)
	}
	t.Setenv("TERMA_ENV", "dev")
	if err := RecordEnv(dir); err != nil {
		t.Fatal(err)
	}
	path := installDefinition(t, stateDir, callerEnv(), same)
	if s := CheckService(stateDir); !s.Installed || !s.Current || s.Path != path {
		t.Fatalf("the service install wrote is not current: %+v", s)
	}

	t.Setenv("TERMA_ENV", "") // a shell without the developer's environment
	if s := CheckService(stateDir); !s.Current {
		t.Fatalf("against the record, a shell's missing TERMA_ENV made the service stale: %+v", s)
	}
	if s := CheckServiceHere(stateDir); s.Current {
		t.Fatalf("against this shell (production) the dev service read as current: %+v", s)
	}
}

// RefreshService leaves alone a service that is not installed or is current, without
// touching the system's service manager.
func TestRefreshServiceLeavesACurrentOrMissingServiceAlone(t *testing.T) {
	if !service.Supported() {
		t.Skip("no relay service on this platform")
	}
	stateDir, dir := sandboxService(t)
	if _, changed, err := RefreshService(t.Context(), stateDir); changed || err != nil {
		t.Fatalf("refreshed a service that is not installed: %v, %v", changed, err)
	}
	t.Setenv("TERMA_ENV", "dev")
	if err := RecordEnv(dir); err != nil {
		t.Fatal(err)
	}
	path := installDefinition(t, stateDir, callerEnv(), same)
	before, _ := os.ReadFile(path)
	t.Setenv("TERMA_ENV", "")
	if _, changed, err := RefreshService(t.Context(), stateDir); changed || err != nil {
		t.Fatalf("refreshed a current service: %v, %v", changed, err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("a refresh rewrote a current service in the caller's environment")
	}
}

// A service an earlier terma wrote reads as stale, so update rewrites it.
func TestAnEarlierTermasServiceIsStale(t *testing.T) {
	if !service.Supported() {
		t.Skip("no relay service on this platform")
	}
	stateDir, _ := sandboxService(t)
	installDefinition(t, stateDir, callerEnv(), func(def string) string {
		return def[:len(def)/2] + "<!-- relay serve -->" + def[len(def)/2:]
	})
	if s := CheckService(stateDir); !s.Installed || s.Current {
		t.Fatalf("an earlier terma's service reads as current: %+v", s)
	}
}

// The running relay records its environment and whether it is the service's, and the
// record goes with it.
func TestARunningRelayRecordsItsEnvironment(t *testing.T) {
	t.Parallel()
	stateDir, dir, _ := setUpRelay(t)
	for _, tc := range []struct {
		name    string
		idle    time.Duration
		service bool
	}{{"hook-started", time.Hour, false}, {"started by hand, never idling", 0, false}, {"service", 0, true}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			c := runConfig(stateDir, tc.idle, nil)
			c.Environment, c.Version, c.Service = "dev", "1.2.0", tc.service
			done := make(chan error, 1)
			go func() { _, err := Run(ctx, c); done <- err }()
			var info RunInfo
			for ok := false; !ok; {
				if ctx.Err() != nil {
					t.Fatal("the relay never recorded itself")
				}
				time.Sleep(10 * time.Millisecond)
				info, ok = RunningRelay(dir)
			}
			if info.PID != os.Getpid() || info.Environment != "dev" || info.Service != tc.service || info.Version != "1.2.0" {
				t.Fatalf("recorded %+v", info)
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if _, ok := RunningRelay(dir); ok {
				t.Fatal("a stopped relay still reads as running")
			}
			if _, err := os.Stat(filepath.Join(dir, RunFile)); !os.IsNotExist(err) {
				t.Fatalf("the record outlived its relay: %v", err)
			}
		})
	}
}

// A record no relay holds the lock for (one that crashed) is not a running relay.
func TestAStaleRunRecordIsNotARunningRelay(t *testing.T) {
	t.Parallel()
	_, dir, _ := setUpRelay(t)
	data, _ := json.Marshal(RunInfo{PID: os.Getpid(), Environment: "dev"})
	if err := os.WriteFile(filepath.Join(dir, RunFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if info, ok := RunningRelay(dir); ok {
		t.Fatalf("a crashed relay's record reads as running: %+v", info)
	}
}
