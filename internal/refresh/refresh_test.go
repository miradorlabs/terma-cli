package refresh

import (
	"errors"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/agentstest"
	"github.com/miradorlabs/terma-cli/internal/project"
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

// A workspace with no binding has nothing a refresh may rewrite.
func TestAnUnboundWorkspaceHasNothingToRefresh(t *testing.T) {
	r := Refresher{Agents: agents.New(agentstest.Agent{ID: "fake"})}
	for _, root := range []string{"", t.TempDir()} {
		if repo, err := r.PlanRepo(root, ""); repo != nil || err != nil {
			t.Fatalf("PlanRepo(%q) = %+v, %v", root, repo, err)
		}
	}
}

// The binding records the build that last wrote the committed files, and only a change
// rewrites it.
func TestStampRecordsTheBuildOnce(t *testing.T) {
	root := t.TempDir()
	if err := project.Save(root, &project.File{Project: project.Project{ID: "p1"}}); err != nil {
		t.Fatal(err)
	}
	r := Refresher{Version: "v2.0.0"}
	if stamped, err := r.Stamp(root); err != nil || !stamped {
		t.Fatalf("Stamp = %v, %v", stamped, err)
	}
	if stamped, err := r.Stamp(root); err != nil || stamped {
		t.Fatalf("a second Stamp = %v, %v", stamped, err)
	}
	if f, err := project.Load(root); err != nil || f.Install.Version != "v2.0.0" {
		t.Fatalf("binding = %+v, %v", f, err)
	}
	if stamped, err := r.Stamp(t.TempDir()); err != nil || stamped {
		t.Fatalf("an unbound checkout was stamped: %v, %v", stamped, err)
	}
}

// The first command under a newer release refreshes once; a failure leaves it due.
func TestAnUpgradeRefreshesOnceAndRetriesAFailure(t *testing.T) {
	dir := t.TempDir()
	failing := Refresher{Version: "v3.0.0", Agents: agents.New(agentstest.Refreshing{Agent: agentstest.Agent{ID: "a"}, Err: errors.New("busy")})}
	if up, err := failing.AfterUpgrade(dir, "", ""); !up.Due || err == nil {
		t.Fatalf("AfterUpgrade = %+v, %v", up, err)
	}
	fine := Refresher{Version: "v3.0.0", Agents: agents.New(agentstest.Refreshing{Agent: agentstest.Agent{ID: "a"}, Path: "/x"})}
	if up, err := fine.AfterUpgrade(dir, "", ""); !up.Due || err != nil || len(up.Changed) != 1 {
		t.Fatalf("the retry = %+v, %v", up, err)
	}
	if up, _ := fine.AfterUpgrade(dir, "", ""); up.Due {
		t.Fatal("a refreshed release refreshed again")
	}
}

// A full refresh reports the machine's files and the repository it planned, and records
// the build only when nothing failed.
func TestARefreshRecordsTheBuildOnlyWhenNothingFailed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TERMA_CONFIG_DIR", dir)
	root := t.TempDir()
	if err := project.Save(root, &project.File{Project: project.Project{ID: "p1"}}); err != nil {
		t.Fatal(err)
	}
	failing := Refresher{Version: "v4.0.0", Agents: agents.New(agentstest.Refreshing{Agent: agentstest.Agent{ID: "a"}, Err: errors.New("busy")})}
	res, err := failing.Run(t.Context(), root, "")
	if err == nil || res.Repo == nil || res.Repo.Root != root {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if !selfupdate.NeedsRefresh(dir, "v4.0.0") {
		t.Fatal("a failed refresh was recorded")
	}
	fine := Refresher{Version: "v4.0.0", Agents: agents.New(agentstest.Refreshing{Agent: agentstest.Agent{ID: "a"}, Path: "/x"})}
	if res, err := fine.Run(t.Context(), "", ""); err != nil || res.Repo != nil || len(res.Machine) != 1 {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if selfupdate.NeedsRefresh(dir, "v4.0.0") {
		t.Fatal("a clean refresh was not recorded")
	}
}
