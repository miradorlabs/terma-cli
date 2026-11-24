package refresh

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/agentstest"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
)

// A machine refresh carries on past an agent that fails, and names it.
func TestTheMachineRefreshCarriesOnPastAFailure(t *testing.T) {
	t.Parallel()
	r := Refresher{Version: "v1.2.3", Agents: agents.New(
		agentstest.Refreshing{Agent: agentstest.Agent{ID: "broken"}, Err: errors.New("permission denied")},
		agentstest.Refreshing{Agent: agentstest.Agent{ID: "fine"}, Path: "/home/dev/.fine/settings.json"},
		agentstest.Refreshing{Agent: agentstest.Agent{ID: "current"}},
	)}
	changed, err := r.Machine()
	if len(changed) != 1 || changed[0] != "/home/dev/.fine/settings.json" {
		t.Fatalf("changed = %v", changed)
	}
	if err == nil || err.Error() != "broken: permission denied" {
		t.Fatalf("err = %v", err)
	}
}

// The first command under a newer release refreshes once; a failure leaves it due.
func TestAnUpgradeRefreshesOnceAndRetriesAFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	failing := Refresher{StateDir: dir, Version: "v3.0.0", Agents: agents.New(agentstest.Refreshing{Agent: agentstest.Agent{ID: "a"}, Err: errors.New("busy")})}
	if up, err := failing.AfterUpgrade(t.Context()); !up.Due || err == nil {
		t.Fatalf("AfterUpgrade = %+v, %v", up, err)
	}
	fine := Refresher{StateDir: dir, Version: "v3.0.0", Agents: agents.New(agentstest.Refreshing{Agent: agentstest.Agent{ID: "a"}, Path: "/x"})}
	if up, err := fine.AfterUpgrade(t.Context()); !up.Due || err != nil || len(up.Changed) != 1 {
		t.Fatalf("the retry = %+v, %v", up, err)
	}
	if up, _ := fine.AfterUpgrade(t.Context()); up.Due {
		t.Fatal("a refreshed release refreshed again")
	}
}

// A full refresh reports the machine's files and records the build only when nothing
// failed.
func TestARefreshRecordsTheBuildOnlyWhenNothingFailed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	failing := Refresher{StateDir: dir, Version: "v4.0.0", Agents: agents.New(agentstest.Refreshing{Agent: agentstest.Agent{ID: "a"}, Err: errors.New("busy")})}
	if _, err := failing.Run(t.Context()); err == nil {
		t.Fatal("Run did not report the failure")
	}
	if !selfupdate.NeedsRefresh(dir, "v4.0.0") {
		t.Fatal("a failed refresh was recorded")
	}
	fine := Refresher{StateDir: dir, Version: "v4.0.0", Agents: agents.New(agentstest.Refreshing{Agent: agentstest.Agent{ID: "a"}, Path: "/x"})}
	if changed, err := fine.Run(t.Context()); err != nil || len(changed) != 1 {
		t.Fatalf("Run = %v, %v", changed, err)
	}
	if selfupdate.NeedsRefresh(dir, "v4.0.0") {
		t.Fatal("a clean refresh was not recorded")
	}
}

// An update rewrites a relay service an earlier build wrote, beside the agents' files, and
// leaves a current one alone.
func TestARefreshRewritesAStaleRelayService(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	calls := 0
	stale := Refresher{StateDir: dir, Version: "v5.0.0", Agents: agents.New(agentstest.Refreshing{Agent: agentstest.Agent{ID: "a"}, Path: "/x"}),
		RelayService: func(context.Context) (string, bool, error) { calls++; return "/svc.plist", true, nil }}
	changed, err := stale.Run(t.Context())
	if err != nil || calls != 1 || !slices.Equal(changed, []string{"/x", "/svc.plist"}) {
		t.Fatalf("Run = %v, %v after %d service refreshes", changed, err, calls)
	}
	current := Refresher{StateDir: dir, Version: "v5.0.0", Agents: agents.New(agentstest.Agent{ID: "a"}),
		RelayService: func(context.Context) (string, bool, error) { return "/svc.plist", false, nil }}
	if changed, err := current.Run(t.Context()); err != nil || len(changed) != 0 {
		t.Fatalf("a current service was reported refreshed: %v, %v", changed, err)
	}
}

// A service that cannot be rewritten fails the refresh, naming the relay, so the release
// stays due and the next command retries; the agents' files are still refreshed.
func TestAFailedRelayServiceRefreshIsRetried(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	broken := Refresher{StateDir: dir, Version: "v6.0.0", Agents: agents.New(agentstest.Refreshing{Agent: agentstest.Agent{ID: "a"}, Path: "/x"}),
		RelayService: func(context.Context) (string, bool, error) {
			return "", false, errors.New("launchctl bootstrap: denied")
		}}
	up, err := broken.AfterUpgrade(t.Context())
	if !up.Due || err == nil || !strings.Contains(err.Error(), "relay service: launchctl bootstrap: denied") || !slices.Equal(up.Changed, []string{"/x"}) {
		t.Fatalf("AfterUpgrade = %+v, %v", up, err)
	}
	if !selfupdate.NeedsRefresh(dir, "v6.0.0") {
		t.Fatal("a failed service refresh was recorded as done")
	}
	fixed := broken
	fixed.RelayService = func(context.Context) (string, bool, error) { return "/svc.plist", true, nil }
	if up, err := fixed.AfterUpgrade(t.Context()); err != nil || !slices.Contains(up.Changed, "/svc.plist") {
		t.Fatalf("the retry = %+v, %v", up, err)
	}
}

// Machine refresh updates already-installed machine-wide hooks files to the current
// build's hook set without creating files for agents that were never set up.
func TestMachineRefreshUpdatesInstalledUserHooks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	installedPath := filepath.Join(dir, "installed", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(installedPath), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"[ -x '/usr/local/bin/terma' ] && '/usr/local/bin/terma' hook --user old-event || true"}]}]}}`
	if err := os.WriteFile(installedPath, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	absentPath := filepath.Join(dir, "absent", "hooks.json")
	installed := agentstest.UserHooked{Agent: agentstest.Agent{ID: "installed"}, Path: installedPath}
	absent := agentstest.UserHooked{Agent: agentstest.Agent{ID: "absent"}, Path: absentPath}
	r := Refresher{
		Version: "v7.0.0",
		Agents:  agents.New(installed, absent).Support(installed, absent),
	}
	changed, err := r.Machine()
	if err != nil {
		t.Fatalf("Machine: %v", err)
	}
	if !slices.Equal(changed, []string{installedPath}) {
		t.Fatalf("changed = %v, want [%s]", changed, installedPath)
	}
	got, err := os.ReadFile(installedPath)
	if err != nil || !strings.Contains(string(got), "hook --user installed-stop") {
		t.Fatalf("refreshed hooks file = %q, %v", got, err)
	}
	if _, err := os.Stat(absentPath); !os.IsNotExist(err) {
		t.Fatalf("absent hooks file should not have been created: err=%v", err)
	}
}

type trustingUserHooked struct {
	agentstest.UserHooked
	syncCalls int
	syncFn    func(hooksFile string, command func(event string) string) (agents.TrustSync, error)
}

func (a *trustingUserHooked) SyncHookTrust(hooksFile string, command func(event string) string) (agents.TrustSync, error) {
	a.syncCalls++
	return a.syncFn(hooksFile, command)
}

// A SyncHookTrust failure fails the refresh so the release remains due, and a retry still
// reconciles hook trust even when the hooks file plan is already empty.
func TestMachineRefreshPropagatesHookTrustFailureAndRetriesWhenPlanEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stateDir := t.TempDir()
	hooksPath := filepath.Join(dir, "hooks.json")
	stale := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"[ -x '/usr/local/bin/terma' ] && '/usr/local/bin/terma' hook --user old-event || true"}]}]}}`
	if err := os.WriteFile(hooksPath, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	failTrust := true
	ag := &trustingUserHooked{
		UserHooked: agentstest.UserHooked{Agent: agentstest.Agent{ID: "trusting"}, Path: hooksPath},
		syncFn: func(string, func(string) string) (agents.TrustSync, error) {
			if failTrust {
				return agents.TrustSync{}, errors.New("config.toml locked")
			}
			return agents.TrustSync{Approved: 1}, nil
		},
	}
	r := Refresher{
		StateDir: stateDir,
		Version:  "v8.0.0",
		Agents:   agents.New(ag).Support(ag),
	}
	if _, err := r.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "config.toml locked") {
		t.Fatalf("first Run = %v, want trust failure", err)
	}
	if !selfupdate.NeedsRefresh(stateDir, "v8.0.0") {
		t.Fatal("failed SyncHookTrust was recorded as refreshed")
	}
	// Retry: the hooks file is now up-to-date (plan.Empty()), yet SyncHookTrust must still run.
	failTrust = false
	changed, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("retry Run: %v", err)
	}
	if ag.syncCalls != 2 {
		t.Fatalf("syncCalls = %d, want 2", ag.syncCalls)
	}
	if !slices.Equal(changed, []string{hooksPath}) {
		t.Fatalf("retry changed = %v, want [%s]", changed, hooksPath)
	}
	if selfupdate.NeedsRefresh(stateDir, "v8.0.0") {
		t.Fatal("successful retry was not recorded as refreshed")
	}
}
