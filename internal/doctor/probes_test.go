package doctor

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/agentstest"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// listed is a git repository in a folder called one, origin github.com/acme/one, which the
// team's policy lists.
func listed(t *testing.T) (root, gitDir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	root = filepath.Join(t.TempDir(), "one")
	if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", root, "remote", "add", "origin", "git@github.com:acme/one.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v\n%s", err, out)
	}
	return root, filepath.Join(root, ".git")
}

func env(t *testing.T, p Probes) Env {
	root, gitDir := listed(t)
	if p.Deliver == nil {
		p.Deliver = func(context.Context) (Delivery, error) { return Delivery{}, nil }
	}
	p.Endpoint = func(string) string { return "https://otlp.example" }
	cfg := &config.Config{Environment: config.EnvProd, ProjectID: "p1", ProjectName: "One",
		Policy: fetched(config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/acme/one"}})}
	return Env{Agents: agents.New(agentstest.Agent{ID: "fake"}), Config: cfg, Root: root, GitDir: gitDir, Probes: p}
}

// fetched is p as setup stores it: validated for a team.
func fetched(p config.Policy) config.Policy {
	p.TeamID, p.FetchedAt = "p1", time.Now()
	return p
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
	if !strings.Contains(rows["Account"], "not signed in") || !strings.Contains(rows["Repository"], "github.com/acme/one is in the team's repositories") ||
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

// A repository the team lists, or any folder in global mode, passes; another warns, naming
// the origin to add to the list.
func TestDoctorChecksTheRepositoryList(t *testing.T) {
	for name, tc := range map[string]struct {
		pol  config.Policy
		want Status
		text string
	}{
		"listed":   {fetched(config.Policy{Mode: config.ModeRepo, Repositories: []string{"GitHub.com/Acme/One"}}), Pass, "github.com/acme/one is in the team's repositories"},
		"unlisted": {fetched(config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/acme/two"}}), Warn, "ask your team to add github.com/acme/one"},
		"global":   {fetched(config.Policy{Mode: config.ModeGlobal, DefaultProjectID: "p9"}), Pass, "the team chosen at setup (p9)"},
	} {
		t.Run(name, func(t *testing.T) {
			e := env(t, Probes{Credential: signedIn, Spool: func() SpoolState { return SpoolState{Open: true} }})
			e.Config.ProjectID, e.Config.ProjectName = "", ""
			e.Config.Policy = tc.pol
			c := check(Run(t.Context(), e, Progress{}), KeyProject)
			if c.Status != tc.want || !strings.Contains(c.Detail+c.Fix, tc.text) {
				t.Fatalf("repository = %+v, want %s with %q", c, tc.want, tc.text)
			}
		})
	}
}

// A folder outside git, or a repository with no hosted origin, warns and says which.
func TestRepositoryCheckSaysWhyNothingIsListed(t *testing.T) {
	pol := fetched(config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/acme/one"}})
	if c := RepositoryCheck(pol, "", nil); c.Status != Warn || !strings.Contains(c.Detail, "not a git repository") {
		t.Fatalf("outside git = %+v", c)
	}
	_, gitDir := listed(t)
	for _, args := range [][]string{{"set-url", "origin", "/srv/git/one.git"}, {"remove", "origin"}} {
		if out, err := exec.Command("git", append([]string{"-C", filepath.Dir(gitDir), "remote"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git remote %v: %v\n%s", args, err, out)
		}
		if c := RepositoryCheck(pol, gitDir, nil); c.Status != Warn || !strings.Contains(c.Detail, "no origin") {
			t.Fatalf("after git remote %v: %+v", args, c)
		}
	}
}

// The local report agrees: an unlisted repository asks the team for the list.
func TestLocalReportChecksTheRepositoryList(t *testing.T) {
	for mode, wantAsk := range map[string]bool{config.ModeGlobal: false, config.ModeRepo: true} {
		t.Run(mode, func(t *testing.T) {
			e := env(t, Probes{Credential: signedIn})
			e.Config.Policy = fetched(config.Policy{Mode: mode, Repositories: []string{"github.com/acme/two"}})
			rep, err := Local(t.Context(), e)
			if err != nil {
				t.Fatal(err)
			}
			asks := false
			for _, c := range rep.Checks {
				asks = asks || strings.Contains(c.Fix, "ask your team")
			}
			if asks != wantAsk {
				t.Fatalf("%s mode: asks for the list = %v, rows %v", mode, asks, rep.Rows)
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

// Doctor admits a repository only under the policy hooks apply: a list not validated for a
// team admits nothing, and a validated empty list says who can change that.
func TestDoctorRepositoryCheckUsesThePolicyHooksApply(t *testing.T) {
	t.Setenv("TERMA_POLICY_STUB", "")
	for _, tc := range []struct {
		name    string
		policy  config.Policy
		fix     string
		passing bool
	}{
		{"validated, listed", config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/acme/one"}, TeamID: "p1", FetchedAt: time.Now()}, "", true},
		{"listed but never fetched", config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/acme/one"}}, "terma setup", false},
		{"global but never fetched", config.Policy{Mode: config.ModeGlobal}, "terma setup", false},
		{"validated, no repositories", config.Policy{Mode: config.ModeRepo, Repositories: []string{" "}, TeamID: "p1", FetchedAt: time.Now()}, NoRepositoriesStep, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := env(t, Probes{Credential: signedIn, Spool: func() SpoolState { return SpoolState{} }})
			e.Config.Policy = tc.policy
			c := check(Run(t.Context(), e, Progress{}), KeyProject)
			if (c.Status == Pass) != tc.passing || c.Fix != tc.fix {
				t.Fatalf("repository check = %+v", c)
			}
		})
	}
}
