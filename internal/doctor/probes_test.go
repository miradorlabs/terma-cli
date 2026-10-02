package doctor

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/agentstest"
	"github.com/miradorlabs/terma-cli/internal/config"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

// bound is a git repository bound to project p1, with nothing else installed.
func bound(t *testing.T) (root, gitDir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	root = t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := termaproject.Save(root, &termaproject.File{Project: termaproject.Project{ID: "p1", Name: "One"}}); err != nil {
		t.Fatal(err)
	}
	return root, root + "/.git"
}

func env(t *testing.T, p Probes) Env {
	root, gitDir := bound(t)
	if p.Deliver == nil {
		p.Deliver = func(context.Context) (Delivery, error) { return Delivery{}, nil }
	}
	p.Endpoint = func(string) string { return "https://otlp.example" }
	return Env{Agents: agents.New(agentstest.Agent{ID: "fake"}), Config: &config.Config{Environment: config.EnvProd},
		Root: root, GitDir: gitDir, Probes: p}
}

func signedIn() (Credential, error) {
	return Credential{Email: "dev@example.com", OrganizationID: "org_a"}, nil
}

func check(r Report, key string) Check {
	for _, c := range r.Checks {
		if c.Key == key {
			return c
		}
	}
	return Check{Key: "missing"}
}

// The credential is judged from the probe alone: none, or one from another environment,
// sends the developer to `terma setup`.
func TestDoctorJudgesTheSignInFromItsProbe(t *testing.T) {
	for name, tc := range map[string]struct {
		cred   func() (Credential, error)
		status Status
	}{
		"signed in":         {signedIn, Pass},
		"not signed in":     {func() (Credential, error) { return Credential{}, errors.New("no credential") }, Fail},
		"other environment": {func() (Credential, error) { return Credential{OtherEnvironment: true}, nil }, Fail},
		"no probe":          {nil, Fail},
	} {
		t.Run(name, func(t *testing.T) {
			c := check(Run(t.Context(), env(t, Probes{Credential: tc.cred, Spool: func() SpoolState { return SpoolState{} }}), Progress{}), KeyAuth)
			if c.Status != tc.status || tc.status == Fail && c.Fix != "terma setup" {
				t.Fatalf("signed in = %+v", c)
			}
		})
	}
}

// A run under TERMA_ENV that the profile does not record warns: the hooks resolve to the
// profile's environment, so their events wait for a key that never comes.
func TestDoctorWarnsWhenTheProfileRecordsAnotherEnvironment(t *testing.T) {
	e := env(t, Probes{Credential: signedIn, Spool: func() SpoolState { return SpoolState{} }})
	e.Config = &config.Config{Environment: config.EnvDev, ProfileEnvironment: config.EnvProd}
	c := check(Run(t.Context(), e, Progress{}), KeyAuth)
	if c.Status != Warn || c.Fix != "TERMA_ENV=dev terma setup" || !strings.Contains(c.Detail, "records prod") {
		t.Fatalf("signed in = %+v", c)
	}
	e.Config = &config.Config{Environment: config.EnvDev, ProfileEnvironment: config.EnvDev}
	if c := check(Run(t.Context(), e, Progress{}), KeyAuth); c.Status != Pass {
		t.Fatalf("pinned profile = %+v", c)
	}
}

// The spool check fails on a queue that cannot be written, and on a project with no key
// here; only then is it a pass.
func TestDoctorJudgesTheSpoolFromItsProbes(t *testing.T) {
	keyed := Keys(func(agent, project string) string {
		if agent == "" && project == "p1" {
			return "ter_srv_…"
		}
		return ""
	})
	for name, tc := range map[string]struct {
		spool  SpoolState
		keys   Keys
		status Status
		detail string
	}{
		"unwritable": {SpoolState{Open: true, WriteErr: errors.New("read-only file system")}, keyed, Fail, "cannot be written"},
		"keyless":    {SpoolState{Open: true, Queued: 3}, nil, Fail, "no team key"},
		"closed":     {SpoolState{}, keyed, Fail, "cannot open"},
		"healthy":    {SpoolState{Open: true, Queued: 3}, keyed, Pass, "3 queued"},
	} {
		t.Run(name, func(t *testing.T) {
			p := Probes{Credential: signedIn, Keys: tc.keys, Spool: func() SpoolState { return tc.spool }}
			c := check(Run(t.Context(), env(t, p), Progress{}), KeySpool)
			if c.Status != tc.status || !strings.Contains(c.Detail, tc.detail) {
				t.Fatalf("event spool = %+v", c)
			}
		})
	}
}

// Another project's refused delivery only warns; this project's fails.
func TestTheBackendCheckTellsThisProjectFromAnother(t *testing.T) {
	keys := Keys(func(string, string) string { return "ter_srv_…" })
	deliver := func(project string) func(context.Context) (Delivery, error) {
		return func(context.Context) (Delivery, error) {
			return Delivery{Err: errors.New("refused"), Failures: []Failure{{ProjectID: project, Err: errors.New("401"), Refused: true, KeyRefused: true}}}, nil
		}
	}
	if c := BackendCheck(t.Context(), Probes{Keys: keys, Deliver: deliver("p1")}, "p1"); c.Status != Fail {
		t.Fatalf("this project's refusal = %+v", c)
	}
	other := BackendCheck(t.Context(), Probes{Keys: keys, Deliver: deliver("p2"), Endpoint: func(string) string { return "https://otlp.example" }}, "p1")
	if other.Status == Fail {
		t.Fatalf("another project's refusal failed this one: %+v", other)
	}
}

// status says what doctor would, from local state alone.
func TestStatusSaysWhatDoctorWould(t *testing.T) {
	e := env(t, Probes{Credential: func() (Credential, error) { return Credential{}, errors.New("none") },
		Spool: func() SpoolState { return SpoolState{Open: true, Queued: 2} }})
	rep, err := Local(t.Context(), e)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{}
	for _, r := range rep.Rows {
		rows[r.Label] = r.Value
	}
	if !strings.Contains(rows["Account"], "not signed in") || !strings.Contains(rows["Repository"], "One") ||
		!strings.Contains(rows["Spool"], "2 event(s) queued, no team key") {
		t.Fatalf("rows = %v", rows)
	}
	fixes := map[string]bool{}
	for _, c := range rep.Checks {
		fixes[c.Fix] = true
	}
	if !fixes["terma setup"] || !fixes["terma doctor"] {
		t.Fatalf("checks = %+v", rep.Checks)
	}
}

// Global mode needs no install, so an unbound repository passes there and fails only in
// per-repository mode.
func TestDoctorNeedsABindingOnlyInPerRepositoryMode(t *testing.T) {
	for mode, want := range map[string]Status{config.ModeGlobal: Pass, config.ModeRepo: Fail} {
		t.Run(mode, func(t *testing.T) {
			e := env(t, Probes{Credential: signedIn, Spool: func() SpoolState { return SpoolState{Open: true} }})
			if err := termaproject.Remove(e.Root); err != nil {
				t.Fatal(err)
			}
			// An unbound repository resolves no project, so only the policy's id names the team.
			e.Config.ProjectID, e.Config.ProjectName = "", ""
			e.Config.Policy = config.Policy{Mode: mode, DefaultProjectID: "p9"}
			c := check(Run(t.Context(), e, Progress{}), KeyProject)
			if c.Status != want || want == Pass && !strings.Contains(c.Detail, "the team chosen at setup (p9)") || want == Fail && c.Fix != "terma install" {
				t.Fatalf("repository bound in %s mode = %+v", mode, c)
			}
		})
	}
}

// The local report agrees: in global mode an unbound repository asks for no install.
func TestLocalReportNeedsABindingOnlyInPerRepositoryMode(t *testing.T) {
	for mode, wantInstall := range map[string]bool{config.ModeGlobal: false, config.ModeRepo: true} {
		t.Run(mode, func(t *testing.T) {
			e := env(t, Probes{Credential: signedIn})
			if err := termaproject.Remove(e.Root); err != nil {
				t.Fatal(err)
			}
			e.Config.Policy = config.Policy{Mode: mode}
			rep, err := Local(t.Context(), e)
			if err != nil {
				t.Fatal(err)
			}
			asks := false
			for _, c := range rep.Checks {
				asks = asks || c.Fix == "terma install"
			}
			if asks != wantInstall {
				t.Fatalf("%s mode: asks for terma install = %v, rows %v", mode, asks, rep.Rows)
			}
		})
	}
}

// A name is shown only for the project the command resolved, never put on another id.
func TestGlobalDestinationNamesOnlyTheResolvedProject(t *testing.T) {
	for _, tc := range []struct {
		cfg  config.Config
		want string
	}{
		{config.Config{ProjectID: "p9", ProjectName: "Engineering", Policy: config.Policy{DefaultProjectID: "p9"}}, "report to Engineering"},
		{config.Config{ProjectID: "p1", ProjectName: "Other", Policy: config.Policy{DefaultProjectID: "p9"}}, "setup (p9)"},
		{config.Config{}, "the team chosen at setup"},
	} {
		if got := GlobalDestination(&tc.cfg); !strings.HasSuffix(got, tc.want) {
			t.Errorf("GlobalDestination(%+v) = %q, want suffix %q", tc.cfg, got, tc.want)
		}
	}
}

// An accepted flush is the proof of delivery; an empty queue proves nothing.
func TestTheBackendCheckPassesOnAnAcceptedFlush(t *testing.T) {
	keys := Keys(func(string, string) string { return "ter_srv_…" })
	probes := func(sent int) Probes {
		return Probes{Keys: keys, Endpoint: func(string) string { return "https://otlp.example" },
			Deliver: func(context.Context) (Delivery, error) { return Delivery{Sent: sent, Delivered: "1 event"}, nil }}
	}
	if c := BackendCheck(t.Context(), probes(1), "p1"); c.Status != Pass {
		t.Fatalf("accepted flush = %+v", c)
	}
	if c := BackendCheck(t.Context(), probes(0), "p1"); c.Status != Skip || !c.Inconclusive {
		t.Fatalf("empty queue = %+v", c)
	}
}
