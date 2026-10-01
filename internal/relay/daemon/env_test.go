package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay/service"
)

// sandboxService puts the per-user service directories under a temporary home and the
// config in a temporary directory, so a test never touches the machine's own relay service.
func sandboxService(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("TERMA_RELAY_HOLD", "")
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// The recorded environment is the caller's relay keys, and only those.
func TestRecordEnvKeepsOnlyWhatPlacesTheRelay(t *testing.T) {
	dir := sandboxService(t)
	if _, ok := RecordedEnv(dir); ok {
		t.Fatal("an environment is recorded before anything recorded one")
	}
	t.Setenv("TERMA_ENV", "dev")
	t.Setenv("TERMA_API_KEY", "secret")
	if err := RecordEnv(dir); err != nil {
		t.Fatal(err)
	}
	env, ok := RecordedEnv(dir)
	if !ok || env["TERMA_ENV"] != "dev" || env["TERMA_CONFIG_DIR"] != os.Getenv("TERMA_CONFIG_DIR") {
		t.Fatalf("recorded %v, %v", env, ok)
	}
	if _, leaked := env["TERMA_API_KEY"]; leaked {
		t.Fatal("RecordEnv recorded a credential")
	}
	if info, err := os.Stat(filepath.Join(dir, EnvFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the record is not private: %v, %v", info, err)
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
	dir := sandboxService(t)
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
	fresh := sandboxService(t)
	if proc := spawnCommand("/opt/terma/bin/terma", fresh); proc.Env != nil {
		t.Fatalf("with nothing recorded the relay should inherit, got %v", proc.Env)
	}
}

// installDefinition writes the definition this terma would install in env, as if installed.
func installDefinition(t *testing.T, env map[string]string, edit func(string) string) string {
	t.Helper()
	m, err := serviceWith(env)
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
	dir := sandboxService(t)
	if s := CheckService(); s.Installed || s.Current {
		t.Fatalf("no service is installed, got %+v", s)
	}
	t.Setenv("TERMA_ENV", "dev")
	if err := RecordEnv(dir); err != nil {
		t.Fatal(err)
	}
	path := installDefinition(t, callerEnv(), same)
	if s := CheckService(); !s.Installed || !s.Current || s.Path != path {
		t.Fatalf("the service install wrote is not current: %+v", s)
	}

	t.Setenv("TERMA_ENV", "") // a shell without the developer's environment
	if s := CheckService(); !s.Current {
		t.Fatalf("against the record, a shell's missing TERMA_ENV made the service stale: %+v", s)
	}
	if s := CheckServiceHere(); s.Current {
		t.Fatalf("against this shell (production) the dev service read as current: %+v", s)
	}
}

// RefreshService leaves alone a service that is not installed or is current, without
// touching the system's service manager.
func TestRefreshServiceLeavesACurrentOrMissingServiceAlone(t *testing.T) {
	if !service.Supported() {
		t.Skip("no relay service on this platform")
	}
	dir := sandboxService(t)
	if _, changed, err := RefreshService(t.Context()); changed || err != nil {
		t.Fatalf("refreshed a service that is not installed: %v, %v", changed, err)
	}
	t.Setenv("TERMA_ENV", "dev")
	if err := RecordEnv(dir); err != nil {
		t.Fatal(err)
	}
	path := installDefinition(t, callerEnv(), same)
	before, _ := os.ReadFile(path)
	t.Setenv("TERMA_ENV", "")
	if _, changed, err := RefreshService(t.Context()); changed || err != nil {
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
	sandboxService(t)
	installDefinition(t, callerEnv(), func(def string) string {
		return def[:len(def)/2] + "<!-- relay serve -->" + def[len(def)/2:]
	})
	if s := CheckService(); !s.Installed || s.Current {
		t.Fatalf("an earlier terma's service reads as current: %+v", s)
	}
}

// The running relay records its environment and whether it is the service's, and the
// record goes with it.
func TestARunningRelayRecordsItsEnvironment(t *testing.T) {
	dir, _ := setUpRelay(t)
	for _, tc := range []struct {
		name    string
		idle    time.Duration
		service bool
	}{{"hook-started", time.Hour, false}, {"service", 0, true}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			c := runConfig(dir, tc.idle, nil)
			c.Environment = "dev"
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
			if info.PID != os.Getpid() || info.Environment != "dev" || info.Service != tc.service {
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
	dir, _ := setUpRelay(t)
	data, _ := json.Marshal(RunInfo{PID: os.Getpid(), Environment: "dev"})
	if err := os.WriteFile(filepath.Join(dir, RunFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if info, ok := RunningRelay(dir); ok {
		t.Fatalf("a crashed relay's record reads as running: %+v", info)
	}
}
