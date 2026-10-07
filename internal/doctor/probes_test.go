package doctor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/agentstest"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// listed is a git repository in a folder called one, origin github.com/acme/one, which the
// team's policy lists.
func listed(t *testing.T) (root, gitDir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
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
	return Env{ConfigDir: t.TempDir(), StateDir: t.TempDir(), Agents: agents.New(agentstest.Agent{ID: "fake"}), Config: cfg, Root: root, GitDir: gitDir, Probes: p}
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
	t.Parallel()
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

// A locked keychain is not a missing sign-in: doctor says which, and says where a working
// credential is kept.
func TestDoctorTellsALockedKeychainFromNoSignIn(t *testing.T) {
	t.Parallel()
	locked := func() (Credential, error) { return Credential{Locked: true}, nil }
	c := check(Run(t.Context(), env(t, Probes{Credential: locked, Spool: func() SpoolState { return SpoolState{} }}), Progress{}), KeyAuth)
	if c.Status != Fail || !strings.Contains(c.Detail, "keychain") || !strings.Contains(c.Fix, "unlock") {
		t.Fatalf("locked keychain = %+v", c)
	}
	for cred, want := range map[Credential]string{
		{Email: "dev@example.com", OrganizationID: "org_a"}:               "credentials in the system keychain",
		{Email: "dev@example.com", OrganizationID: "org_a", InFile: true}: "credentials in plain text",
	} {
		probe := func() (Credential, error) { return cred, nil }
		if c := check(Run(t.Context(), env(t, Probes{Credential: probe, Spool: func() SpoolState { return SpoolState{} }}), Progress{}), KeyAuth); c.Status != Pass || !strings.Contains(c.Detail, want) {
			t.Fatalf("signed in = %+v, want %q", c, want)
		}
	}
}

// A run under TERMA_ENV that the profile does not record warns: the hooks resolve to the
// profile's environment, so their events wait for a key that never comes.
func TestDoctorWarnsWhenTheProfileRecordsAnotherEnvironment(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	for name, tc := range map[string]struct {
		pol  config.Policy
		want Status
		text string
	}{
		"listed":   {fetched(config.Policy{Mode: config.ModeRepo, Repositories: []string{"GitHub.com/Acme/One"}}), Pass, "github.com/acme/one is in the team's repositories"},
		"unlisted": {fetched(config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/acme/two"}}), Warn, "ask a team admin to add github.com/acme/one"},
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
	t.Parallel()
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

// An SSH host alias is never offered to the team, whose list saves only a domain or
// localhost; doctor says how to point origin at the real host instead.
func TestRepositoryCheckAdvisesOnAnSSHAlias(t *testing.T) {
	t.Parallel()
	_, gitDir := listed(t)
	pol := fetched(config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/acme/one"}})
	for origin, alias := range map[string]bool{
		"git@github-work:acme/one.git":          true,
		"ssh://git@github-work:22/acme/one":     true,
		"git@github.com:acme/two.git":           false,
		"ssh://git@localhost:2222/acme/one.git": false,
	} {
		if out, err := exec.Command("git", "-C", filepath.Dir(gitDir), "remote", "set-url", "origin", origin).CombinedOutput(); err != nil {
			t.Fatalf("git remote set-url: %v\n%s", err, out)
		}
		c := RepositoryCheck(pol, gitDir, nil)
		advised := strings.Contains(c.Fix, "git remote set-url origin 'git@<real host>:acme/one.git'") && strings.Contains(c.Fix, "core.sshCommand")
		if c.Status != Warn || advised != alias || alias && strings.Contains(c.Fix, "ask a team admin") {
			t.Errorf("%s: %+v", origin, c)
		}
	}
}

// The local report agrees: an unlisted repository asks a team admin for the list.
func TestLocalReportChecksTheRepositoryList(t *testing.T) {
	t.Parallel()
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
				asks = asks || strings.Contains(c.Fix, "ask a team admin")
			}
			if asks != wantAsk {
				t.Fatalf("%s mode: asks for the list = %v, rows %v", mode, asks, rep.Rows)
			}
		})
	}
}

// A name is shown only for the project the command resolved, never put on another id.
func TestGlobalDestinationNamesOnlyTheResolvedProject(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
		{"validated, no policy set", config.Policy{Mode: config.ModeRepo, Unset: true, TeamID: "p1", FetchedAt: time.Now()}, NoPolicyStep, false},
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

// Once the policy has expired, capture is off: doctor and status no longer report the
// repository collected or the agent exporting as working, and point at the cause.
func TestCaptureOffWarnsTheCollectionChecks(t *testing.T) {
	t.Parallel()
	for name, age := range map[string]time.Duration{"fresh": 0, "expired": config.MaxPolicyAge + time.Hour} {
		t.Run(name, func(t *testing.T) {
			r := healthy()
			r.Environment = config.EnvProd
			e := env(t, Probes{Credential: signedIn, Keys: keyed, Relay: func() Relay { return r }, Spool: func() SpoolState { return SpoolState{} }})
			token := claim.TokenPath(e.StateDir)
			if err := os.MkdirAll(filepath.Dir(token), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(token, []byte("local"), 0o600); err != nil {
				t.Fatal(err)
			}
			e.Config.Policy.FetchedAt = time.Now().Add(-age)
			rep := Run(t.Context(), e, Progress{})
			for _, key := range []string{KeyProject, KeyHarness} {
				c := check(rep, key)
				if age == 0 && c.Status != Pass {
					t.Fatalf("%s with a fresh policy: %+v", key, c)
				}
				if age > 0 && (c.Status != Warn || !strings.Contains(c.Detail, "capture is off") || c.Fix != "terma setup") {
					t.Fatalf("%s with capture off: %+v", key, c)
				}
			}
			local, err := Local(t.Context(), e)
			if err != nil {
				t.Fatal(err)
			}
			if off := slices.ContainsFunc(local.Checks, func(c Check) bool { return strings.Contains(c.Detail, "capture is off") }); off != (age > 0) {
				t.Fatalf("status reports capture off = %v, checks %+v", off, local.Checks)
			}
		})
	}
}

// A repository another organization's team lists is collected, for that team, and doctor
// says so; the machine's rows name every team it also collects for.
func TestDoctorAdmitsARepositoryByAnotherOrganizationsTeam(t *testing.T) {
	t.Parallel()
	e := env(t, Probes{Credential: signedIn, Spool: func() SpoolState { return SpoolState{} }})
	e.Config.StateDir, e.Config.OrganizationID, e.Config.AuthURL = e.StateDir, "org_a", config.DefaultAuthURL
	e.Config.Teams = map[string]string{"org_a": "p1", "org_b": "p2"}
	e.Config.Policy = fetched(config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/acme/two"}, OrganizationID: "org_a", AuthURL: config.DefaultAuthURL})
	other := config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/acme/one"}, TeamID: "p2", OrganizationID: "org_b", OrganizationName: "Beta",
		AuthURL: config.DefaultAuthURL, IncludePrompts: true, IncludeToolContent: true, FetchedAt: time.Now()}
	for _, p := range []config.Policy{e.Config.Policy, other} {
		if err := config.WritePolicy(e.StateDir, p); err != nil {
			t.Fatal(err)
		}
	}
	c := check(Run(t.Context(), e, Progress{}), KeyProject)
	if c.Status != Pass || !strings.Contains(c.Detail, "collected for team p2 of Beta") {
		t.Fatalf("repository = %+v", c)
	}
	rep, err := Local(t.Context(), e)
	if err != nil {
		t.Fatal(err)
	}
	var also, repo string
	for _, r := range rep.Rows {
		switch r.Label {
		case "Also":
			also = r.Value
		case "Repository":
			repo = r.Value
		}
	}
	if !strings.Contains(also, "github.com/acme/one") || !strings.Contains(also, "team p2 of Beta") || !strings.Contains(repo, "team p2 of Beta") {
		t.Fatalf("rows %v", rep.Rows)
	}
	for _, c := range rep.Checks {
		if strings.Contains(c.Fix, "ask a team admin") {
			t.Fatalf("asked for the list though another team collects here: %+v", c)
		}
	}
}

// The machine's rows name a selected team whose policy is not validated, except the
// current organization's own, which the repository check reports.
func TestMachineRowsNameOtherTeamsWithoutAPolicy(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{OrganizationID: "org_a", AuthURL: config.DefaultAuthURL, Policy: config.NoPolicy("org_a", config.DefaultAuthURL),
		Teams: map[string]string{"org_a": "pa", "org_b": "pb"}}
	var also []string
	for _, r := range machineRows(cfg, routing.Collect(cfg)) {
		if r.Label == "Also" {
			also = append(also, r.Value)
		}
	}
	if len(also) != 1 || !strings.Contains(also[0], "team pb of org_b") || !strings.Contains(also[0], "terma setup --org org_b") {
		t.Fatalf("Also rows = %q; want org_b's team alone", also)
	}
	// The selected policy names another team than the organization's recorded one (a
	// --team override): neither is listed as another team's.
	cfg.Policy = config.Policy{Mode: config.ModeRepo, TeamID: "px", OrganizationID: "org_a", AuthURL: config.DefaultAuthURL, FetchedAt: time.Now()}
	also = nil
	for _, r := range machineRows(cfg, routing.Collect(cfg)) {
		if r.Label == "Also" {
			also = append(also, r.Value)
		}
	}
	if len(also) != 1 || !strings.Contains(also[0], "team pb of org_b") {
		t.Fatalf("Also rows with a team override = %q", also)
	}
}
