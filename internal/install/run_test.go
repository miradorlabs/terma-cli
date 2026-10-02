package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/agentstest"
	"github.com/miradorlabs/terma-cli/internal/config"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

const (
	org  = "org_a"
	auth = "https://auth.example"
)

// workflow records each step it was asked for.
func workflow(log *[]string) Workflow {
	note := func(s string) { *log = append(*log, s) }
	return Workflow{
		SignIn: func(_ context.Context, cfg *config.Config) (*config.Config, error) { note("sign-in"); return cfg, nil },
		Bind: func(context.Context, *config.Config, *termaproject.File, string, bool, bool) (Binding, error) {
			note("bind")
			return Binding{ID: "p1", Name: "One", OrganizationID: org}, nil
		},
		ApplySteps: func(*config.Config, Plan) Steps { note("apply"); return Steps{} },
	}
}

func request(t *testing.T) (*config.Config, Request) {
	t.Helper()
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("TERMA_POLICY_STUB", "")
	cfg := &config.Config{OrganizationID: org, AuthURL: auth, Environment: config.EnvProd}
	return cfg, Request{Root: t.TempDir(), Selected: []string{"fake"}, RecordSelected: true, AssumeYes: true, Version: "v1.0.0", Now: time.Now()}
}

func registry() *agents.Registry { return agents.New(agentstest.Agent{ID: "fake"}) }

// An install signs in, binds, then writes, and records the developer's choice of agents.
// It knows nothing of the team's collection policy: nothing fetches or stores one, and
// none need be reachable.
func TestAnInstallKnowsNothingOfThePolicy(t *testing.T) {
	cfg, req := request(t)
	var log []string
	if _, err := Run(t.Context(), registry(), cfg, req, workflow(&log), &report{}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"sign-in", "bind", "apply"}; !slices.Equal(log, want) {
		t.Fatalf("steps = %v, want %v", log, want)
	}
	if f, err := termaproject.Load(req.Root); err != nil || f.Project.ID != "p1" {
		t.Fatalf("binding = %+v, %v", f, err)
	}
	file, _ := config.LoadFile()
	if file == nil || !slices.Equal(file.Profiles[config.DefaultProfile].Harnesses, []string{"fake"}) {
		t.Fatal("the agents chosen were not recorded")
	}
	if p := file.Profiles[config.DefaultProfile].Policy; p != nil {
		t.Fatalf("install stored a collection policy: %+v", p)
	}
	if dir, _ := config.Dir(); fileExists(filepath.Join(dir, "policies")) {
		t.Fatal("install cached a team's collection policy")
	}
}

// A binding the developer cannot see refuses the install before anything writes.
func TestARefusedInstallWritesNothing(t *testing.T) {
	cfg, req := request(t)
	var log []string
	w := workflow(&log)
	w.Bind = func(context.Context, *config.Config, *termaproject.File, string, bool, bool) (Binding, error) {
		log = append(log, "bind")
		return Binding{}, errors.New("no such team")
	}
	if _, err := Run(t.Context(), registry(), cfg, req, w, &report{}); err == nil {
		t.Fatal("installed")
	}
	if slices.Contains(log, "apply") {
		t.Fatalf("steps = %v", log)
	}
	if _, err := termaproject.Load(req.Root); !errors.Is(err, termaproject.ErrNotFound) {
		t.Fatalf("a binding was written: %v", err)
	}
	if file, _ := config.LoadFile(); file != nil && file.Profiles[config.DefaultProfile] != nil {
		t.Fatal("the agents chosen were recorded")
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// A dry run signs in to nothing, and a signed-out one still plans.
func TestADryRunCallsNoStep(t *testing.T) {
	cfg, req := request(t)
	req.DryRun = true
	var log []string
	w := workflow(&log)
	w.Bind = func(context.Context, *config.Config, *termaproject.File, string, bool, bool) (Binding, error) {
		log = append(log, "bind")
		return Binding{}, ErrNotSignedIn
	}
	if _, err := Run(t.Context(), registry(), cfg, req, w, &report{}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(log, []string{"bind"}) {
		t.Fatalf("a dry run asked for %v", log)
	}
	if _, err := termaproject.Load(req.Root); !errors.Is(err, termaproject.ErrNotFound) {
		t.Fatal("a dry run wrote the binding")
	}
}

// The steps that decide where an install writes are never optional.
func TestAnInstallNeedsItsSteps(t *testing.T) {
	cfg, req := request(t)
	var log []string
	w := workflow(&log)
	w.Bind = nil
	if _, err := Run(t.Context(), registry(), cfg, req, w, &report{}); err == nil || len(log) != 0 {
		t.Fatalf("ran without a binding step: %v, %v", err, log)
	}
}

// An uninstall removes what an install wrote, and says so before it does.
func TestAnUninstallRemovesWhatTheInstallWrote(t *testing.T) {
	cfg, req := request(t)
	var log []string
	if _, err := Run(t.Context(), registry(), cfg, req, workflow(&log), &report{}); err != nil {
		t.Fatal(err)
	}
	rm, err := PlanRemoval(t.Context(), registry(), req.Root, "")
	if err != nil || rm.Empty() || rm.Existing == nil {
		t.Fatalf("PlanRemoval = %+v, %v", rm, err)
	}
	if err := rm.Apply(t.Context(), registry(), func(w string) { t.Error(w) }); err != nil {
		t.Fatal(err)
	}
	if _, err := termaproject.Load(req.Root); !errors.Is(err, termaproject.ErrNotFound) {
		t.Fatalf("the binding survived: %v", err)
	}
	if rm, err := PlanRemoval(t.Context(), registry(), req.Root, ""); err != nil || !rm.Empty() {
		t.Fatalf("a second uninstall found %+v, %v", rm, err)
	}
}
