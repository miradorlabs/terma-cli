package refresh

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/agentstest"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
)

// A machine refresh carries on past an agent that fails, and names it.
func TestTheMachineRefreshCarriesOnPastAFailure(t *testing.T) {
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
	dir := t.TempDir()
	failing := Refresher{Version: "v3.0.0", Agents: agents.New(agentstest.Refreshing{Agent: agentstest.Agent{ID: "a"}, Err: errors.New("busy")})}
	if up, err := failing.AfterUpgrade(t.Context(), dir); !up.Due || err == nil {
		t.Fatalf("AfterUpgrade = %+v, %v", up, err)
	}
	fine := Refresher{Version: "v3.0.0", Agents: agents.New(agentstest.Refreshing{Agent: agentstest.Agent{ID: "a"}, Path: "/x"})}
	if up, err := fine.AfterUpgrade(t.Context(), dir); !up.Due || err != nil || len(up.Changed) != 1 {
		t.Fatalf("the retry = %+v, %v", up, err)
	}
	if up, _ := fine.AfterUpgrade(t.Context(), dir); up.Due {
		t.Fatal("a refreshed release refreshed again")
	}
}

// A full refresh reports the machine's files and records the build only when nothing
// failed.
func TestARefreshRecordsTheBuildOnlyWhenNothingFailed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TERMA_CONFIG_DIR", dir)
	failing := Refresher{Version: "v4.0.0", Agents: agents.New(agentstest.Refreshing{Agent: agentstest.Agent{ID: "a"}, Err: errors.New("busy")})}
	if _, err := failing.Run(t.Context()); err == nil {
		t.Fatal("Run did not report the failure")
	}
	if !selfupdate.NeedsRefresh(dir, "v4.0.0") {
		t.Fatal("a failed refresh was recorded")
	}
	fine := Refresher{Version: "v4.0.0", Agents: agents.New(agentstest.Refreshing{Agent: agentstest.Agent{ID: "a"}, Path: "/x"})}
	if res, err := fine.Run(t.Context()); err != nil || len(res.Machine) != 1 {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if selfupdate.NeedsRefresh(dir, "v4.0.0") {
		t.Fatal("a clean refresh was not recorded")
	}
}

// An update rewrites a relay service an earlier build wrote, beside the agents' files, and
// leaves a current one alone.
func TestARefreshRewritesAStaleRelayService(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	calls := 0
	stale := Refresher{Version: "v5.0.0", Agents: agents.New(agentstest.Refreshing{Agent: agentstest.Agent{ID: "a"}, Path: "/x"}),
		RelayService: func(context.Context) (string, bool, error) { calls++; return "/svc.plist", true, nil }}
	res, err := stale.Run(t.Context())
	if err != nil || calls != 1 || !slices.Equal(res.Machine, []string{"/x", "/svc.plist"}) {
		t.Fatalf("Run = %+v, %v after %d service refreshes", res, err, calls)
	}
	current := Refresher{Version: "v5.0.0", Agents: agents.New(agentstest.Agent{ID: "a"}),
		RelayService: func(context.Context) (string, bool, error) { return "/svc.plist", false, nil }}
	if res, err := current.Run(t.Context()); err != nil || len(res.Machine) != 0 {
		t.Fatalf("a current service was reported refreshed: %+v, %v", res, err)
	}
}

// A service that cannot be rewritten fails the refresh, naming the relay, so the release
// stays due and the next command retries; the agents' files are still refreshed.
func TestAFailedRelayServiceRefreshIsRetried(t *testing.T) {
	dir := t.TempDir()
	broken := Refresher{Version: "v6.0.0", Agents: agents.New(agentstest.Refreshing{Agent: agentstest.Agent{ID: "a"}, Path: "/x"}),
		RelayService: func(context.Context) (string, bool, error) {
			return "", false, errors.New("launchctl bootstrap: denied")
		}}
	up, err := broken.AfterUpgrade(t.Context(), dir)
	if !up.Due || err == nil || !strings.Contains(err.Error(), "relay service: launchctl bootstrap: denied") || !slices.Equal(up.Changed, []string{"/x"}) {
		t.Fatalf("AfterUpgrade = %+v, %v", up, err)
	}
	if !selfupdate.NeedsRefresh(dir, "v6.0.0") {
		t.Fatal("a failed service refresh was recorded as done")
	}
	fixed := broken
	fixed.RelayService = func(context.Context) (string, bool, error) { return "/svc.plist", true, nil }
	if up, err := fixed.AfterUpgrade(t.Context(), dir); err != nil || !slices.Contains(up.Changed, "/svc.plist") {
		t.Fatalf("the retry = %+v, %v", up, err)
	}
}
