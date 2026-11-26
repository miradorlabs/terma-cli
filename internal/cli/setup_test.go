package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/builtin"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
)

// After teardown removes the policies, setup without a terminal reuses the team the profile
// selected, even in an organization with several teams.
func TestSetupAfterTeardownReusesTheSelectedTeam(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	t.Setenv("TERMA_POLICY_STUB", "")
	signInTo(t, gateway, orgA())
	selected := projectsIn(orgA().ID)[1]
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.OrganizationID, p.Team = orgA().ID, selected.ID }); err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(""))
	cmd.SetContext(t.Context())
	if name, err := testApp.selectPolicyTeam(cmd, cfg, false); err != nil || name != selected.Name || cfg.ProjectID != selected.ID {
		t.Fatalf("selectPolicyTeam = %q, %v; project %q, want %s", name, err, cfg.ProjectID, selected.ID)
	}
}

// On a terminal, the team an earlier setup saved is only the picker's default in an
// organization with several, where a team created since is easily the one meant; --team
// and an organization with one team are taken without asking, and a saved team since
// deleted gives way to the only one, or, when setup cannot ask, is refused by name.
func TestSetupOffersTheSavedTeamAsTheDefault(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	t.Setenv("TERMA_POLICY_STUB", "")
	for _, c := range []struct {
		name     string
		org      organization
		explicit bool
		gone     bool
		ask      bool
		asks     bool
	}{
		{"several teams, saved", orgA(), false, false, true, true},
		{"several teams, --team", orgA(), true, false, true, false},
		{"one team, saved", orgB(), false, false, true, false},
		{"one team, saved team deleted", orgB(), false, true, false, false},
		{"several teams, saved team deleted, cannot ask", orgA(), false, true, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			signInTo(t, gateway, c.org)
			saved := projectsIn(c.org.ID)[len(projectsIn(c.org.ID))-1]
			savedID := saved.ID
			if c.gone {
				savedID = "dddddddd-0000-4000-8000-000000000009"
			}
			if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.OrganizationID, p.Team = c.org.ID, savedID }); err != nil {
				t.Fatal(err)
			}
			cfg, err := testApp.loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if c.explicit {
				cfg.ProjectID = saved.ID
			}
			cmd := &cobra.Command{}
			cmd.SetIn(strings.NewReader(""))
			cmd.SetContext(t.Context())
			// No terminal here, so a picker fails: what matters is whether setup asked.
			name, err := testApp.selectPolicyTeam(cmd, cfg, c.ask)
			if asked := err != nil && strings.Contains(err.Error(), "no terminal to prompt on"); asked != c.asks {
				t.Fatalf("selectPolicyTeam = %q, %v; asked %v, want %v", name, err, asked, c.asks)
			}
			if c.gone && len(projectsIn(c.org.ID)) > 1 {
				if err == nil || !strings.Contains(err.Error(), "no longer in this organization") {
					t.Fatalf("selectPolicyTeam = %q, %v; want the deleted team refused by name", name, err)
				}
				return
			}
			if !c.asks && (err != nil || name != saved.Name || cfg.ProjectID != saved.ID) {
				t.Fatalf("selectPolicyTeam = %q, %v; project %q, want %s", name, err, cfg.ProjectID, saved.ID)
			}
		})
	}
}

// setup records the collection policy and points the agents at the relay, no repository involved.
func TestSetupFetchesThePolicyAndPointsAgentsAtTheRelay(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	signInTo(t, gateway, orgA())
	out, err := runTerma(t, "setup", "--harness", "codex")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	loaded, err := testApp.loadConfig()
	if p := loaded.Policy; err != nil || !p.Validated() || p.Mode != config.ModeRepo || !p.IncludePrompts || p.FetchedAt.IsZero() {
		t.Fatalf("policy not recorded: %+v", p)
	}
	if !strings.Contains(out, "Collects      sessions in the team's repositories (github.com/acme/app)") {
		t.Fatalf("setup did not say the policy:\n%s", out)
	}
	// The test binary is no release, so it is never updated; setup says so.
	if !strings.Contains(out, "Updates       never for a development build") {
		t.Fatalf("setup did not say how terma updates:\n%s", out)
	}
	token, err := daemon.Token(testApp.stateDir)
	if err != nil {
		t.Fatalf("no relay token after setup: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil || !strings.Contains(string(data), "127.0.0.1:43180") || !strings.Contains(string(data), token) {
		t.Fatalf("Codex not pointed at the relay: %v\n%s", err, data)
	}
	if out, err := runTerma(t, "setup", "--harness", "codex", "--relay-service", "sometimes"); err == nil {
		t.Fatalf("--relay-service sometimes was accepted:\n%s", out)
	}
}

// A team that lists no repositories collects nothing, and setup says so as a step left to do.
func TestSetupWarnsWhenTheTeamListsNoRepositories(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TERMA_POLICY_STUB", `{"mode":"repo","repositories":[],"include_prompts":true,"include_tool_content":true,"default_project_id":"team"}`)
	signInTo(t, gateway, orgA())
	out, err := runTerma(t, "setup", "--harness", "codex")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	for _, want := range []string{"! Collects      nothing yet: your team lists no repositories", "Next steps:", doctor.NoRepositoriesStep} {
		if !strings.Contains(out, want) {
			t.Errorf("setup output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "✓ Setup complete") {
		t.Errorf("setup called itself complete while collecting nothing:\n%s", out)
	}
}

// A team no admin has given a collection policy collects nothing; setup and doctor say the
// policy does not exist yet and who sets it up, not that the team lists no repositories.
func TestSetupAndDoctorSayWhenTheTeamHasNoPolicy(t *testing.T) {
	gateway := newFakeAuth(t)
	gateway.orgs = []organization{orgB()}
	gateway.policyBody = `{}`
	authSandbox(t, gateway)
	sandboxMachine(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TERMA_POLICY_STUB", "")
	signInTo(t, gateway, orgB())
	out, err := runTerma(t, "setup", "--harness", "codex")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	for _, want := range []string{"! Collects      nothing yet: your team has no collection policy", "Next steps:", doctor.NoPolicyStep} {
		if !strings.Contains(out, want) {
			t.Errorf("setup output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "lists no repositories") || strings.Contains(out, doctor.NoRepositoriesStep) {
		t.Errorf("setup blamed an empty repository list:\n%s", out)
	}
	// No policy has git_hooks off too, but the step is the policy, not commit hooks.
	if strings.Contains(out, doctor.GitHooksOffStep) {
		t.Errorf("setup recommended commit hooks to a team with no policy:\n%s", out)
	}
	loaded, err := testApp.loadConfig()
	if err != nil || !loaded.Policy.Unset || !loaded.Policy.AdmitsNone() {
		t.Fatalf("stored policy = %+v, %v; want unset and admitting nothing", loaded.Policy, err)
	}
	// Doctor names the team and organization it asked, to hold against the web app's.
	out, _ = runTerma(t, "doctor")
	if !regexp.MustCompile(`repository collected +team "Beta Core" \(bbbbbbbb\) in Beta Labs has no collection policy\n`).MatchString(out) || !strings.Contains(out, doctor.NoPolicyStep) {
		t.Errorf("doctor did not say which team has no policy:\n%s", out)
	}
}

// A team policy whose list holds a wildcard is stored as 2.x: setup on a fresh machine (the
// desktop's connect step runs the same setup) reads it, says a wildcard collects every
// repository under its owner, and keeps it marked so hooks and the relay admit by it.
func TestSetupReadsAWildcardPolicy(t *testing.T) {
	gateway := newFakeAuth(t)
	gateway.orgs = []organization{orgB()}
	gateway.policyBody = `{"policy":{"version":"2.0","terma":{"capture":{"exclude_prompts":false,"exclude_tool_content":false},"git_hooks":true,"per_repository":{"repositories":["github.com/Acme/*","github.com/other/api"]}}},"revision":4,"updated_at":"2026-11-19T00:00:00Z"}`
	authSandbox(t, gateway)
	sandboxMachine(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TERMA_POLICY_STUB", "")
	signInTo(t, gateway, orgB())
	out, err := runTerma(t, "setup", "--harness", "codex")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if want := "Collects      sessions in the team's repositories (every repository under github.com/Acme, github.com/other/api)"; !strings.Contains(out, want) {
		t.Errorf("setup output lacks %q:\n%s", want, out)
	}
	loaded, err := testApp.loadConfig()
	if err != nil || !loaded.Policy.Wildcards || loaded.Policy.Revision != 4 {
		t.Fatalf("stored policy = %+v, %v; want the 2.0 policy, marked Wildcards", loaded.Policy, err)
	}
	if !loaded.Policy.Admits(config.Repository{Origin: "github.com/acme/web"}) || loaded.Policy.Admits(config.Repository{Origin: "github.com/someone/web"}) {
		t.Errorf("the stored policy does not admit by its wildcard: %+v", loaded.Policy)
	}
}

func TestHarnessSelectionComingSoon(t *testing.T) {
	chosen := map[string]bool{}
	for _, name := range testApp.agents.Names() {
		chosen[name] = true
	}
	form := testApp.harnessSelectionForm(chosen)
	wantOrder := []string{"claude", "codex", "copilot", "omp", "antigravity", "grok", "cursor", "opencode", "pi", "hermes", "gemini", "dsh"}
	if !testApp.agents.IsSupported("antigravity") {
		// Coming Soon on Windows: last, in registry order.
		wantOrder = append(slices.Delete(wantOrder, 4, 6), "antigravity", "grok")
	}
	if len(form.Items) != len(wantOrder) {
		t.Fatalf("picker has %d items, want %d", len(form.Items), len(wantOrder))
	}
	// The supported agents lead the list, each selectable, under the name setup shows.
	leading := []string{"Claude Code & Desktop", "Codex TUI & Desktop", "Copilot (beta)", "OMP 18.18.2+ (beta)"}
	if testApp.agents.IsSupported("antigravity") {
		leading = append(leading, "Antigravity 1.2.4+ (beta)", "Grok Build (beta)")
	}
	for i, label := range leading {
		if item := form.Items[i]; item.Label != label || item.Disabled || !testApp.agents.IsSupported(wantOrder[i]) {
			t.Errorf("picker row %d = %+v, want %q supported and selectable", i, item, label)
		}
	}
	for i, name := range wantOrder {
		// A Coming Soon agent is listed under its own name, without a surface's marker.
		display := name
		if a, ok := testApp.agents.Lookup(name); ok && !testApp.agents.IsSupported(name) {
			display = a.DisplayName()
		} else if s, _, ok := testApp.agents.Surface(name); ok {
			display = s.DisplayName
		}
		if form.Items[i].Label != display {
			t.Errorf("picker row %d = %q, want %q", i, form.Items[i].Label, display)
		}
		item := form.Items[i]
		available := testApp.agents.IsSupported(name)
		if available {
			if item.Disabled || !item.Selected || item.Detail != "detected" {
				t.Errorf("%s should be selectable, ticked and marked detected: %+v", name, item)
			}
		} else if !item.Disabled || item.Selected || item.Reason != "Coming Soon" {
			t.Errorf("%s should be disabled, unselected, and labeled Coming Soon: %+v", name, item)
		}
	}
}

// On Windows Antigravity and Grok Build are Coming Soon, listed under their own names:
// "(beta)" marks what setup offers, and it offers them on macOS and Linux only.
func TestHarnessSelectionAntigravityOnWindows(t *testing.T) {
	app := New(func(dir string) *agents.Registry { return builtin.AgentsOn(dir, "windows") }, "dev")
	app.binDirs = func() []string { return nil }
	app.configure(t.TempDir(), t.TempDir())
	form := app.harnessSelectionForm(map[string]bool{"antigravity": true, "grok": true})
	last := len(form.Items) - 2
	for i, label := range []string{"Antigravity", "Grok Build"} {
		item := form.Items[last+i]
		if item.Label != label || !item.Disabled || item.Selected || item.Reason != "Coming Soon" {
			t.Fatalf("%s's row on Windows = %+v, want plain, disabled and Coming Soon", label, item)
		}
		for _, other := range form.Items[:last] {
			if strings.Contains(other.Label, label) {
				t.Fatalf("%s listed twice: %+v", label, other)
			}
		}
	}
}

func TestHarnessSelectionFlags(t *testing.T) {
	for _, name := range []string{"cursor", "opencode"} {
		t.Run(name, func(t *testing.T) {
			if _, err := testApp.chooseHarnesses(&cobra.Command{}, setupFlags{harnesses: "claude," + name}); err == nil || !strings.Contains(err.Error(), "Coming Soon") {
				t.Fatalf("setup error = %v", err)
			}
		})
	}
	got, err := testApp.parseAgentList("codex,claude,codex")
	if err != nil || !slices.Equal(got, []string{"claude", "codex"}) {
		t.Fatalf("available selection = %v, %v", got, err)
	}
	if got, err := testApp.chooseHarnesses(&cobra.Command{}, setupFlags{harnesses: "omp,claude"}); err != nil || !slices.Equal(got, []string{"claude", "omp"}) {
		t.Fatalf("--harness omp,claude = %v, %v", got, err)
	}
	for _, name := range []string{"antigravity", "grok"} {
		got, err = testApp.chooseHarnesses(&cobra.Command{}, setupFlags{harnesses: name + ",claude"})
		if !testApp.agents.IsSupported(name) {
			if err == nil || !strings.Contains(err.Error(), "Coming Soon") {
				t.Fatalf("%s on Windows: %v, %v", name, got, err)
			}
		} else if err != nil || !slices.Equal(got, []string{"claude", name}) {
			t.Fatalf("%s selection = %v, %v", name, got, err)
		}
	}
}

// Only the agents detected on this machine are ticked, and marked so; an agent recorded by
// an earlier setup but not detected now is neither ticked nor recorded by --yes.
func TestHarnessSelectionTicksOnlyDetectedAgents(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	ctx := context.Background()
	var detected []string
	for _, n := range testApp.availableAgentNames() {
		if s, _, _ := testApp.agents.Surface(n); s.Installed(ctx) {
			detected = append(detected, n)
		}
	}
	// Antigravity and grok are not on the emptied PATH.
	if slices.Contains(detected, "grok") {
		t.Fatalf("grok detected with an empty PATH: %v", detected)
	}
	got, err := testApp.chooseHarnesses(&cobra.Command{}, setupFlags{assumeYes: true})
	if err != nil || !slices.Equal(got, detected) {
		t.Fatalf("--yes selection = %v, %v; want the detected %v", got, err, detected)
	}

	prevAsk, prevForm := testApp.canAsk, testApp.runForm
	t.Cleanup(func() { testApp.canAsk, testApp.runForm = prevAsk, prevForm })
	testApp.canAsk = func() bool { return true }
	var offered []prompt.Item
	testApp.runForm = func(form *prompt.Form) ([]prompt.Item, error) {
		offered = slices.Clone(form.Items)
		return form.Items, nil
	}
	got, err = testApp.chooseHarnesses(&cobra.Command{}, setupFlags{})
	if err != nil || !slices.Equal(got, detected) {
		t.Fatalf("picker selection = %v, %v; want the detected %v", got, err, detected)
	}
	for i, a := range testApp.harnessSelectionAgents() {
		item := offered[i]
		want := slices.Contains(detected, a.Name())
		if item.Selected != want || (item.Detail == "detected") != want {
			t.Errorf("%s row = %+v; want ticked and marked detected = %v", a.Name(), item, want)
		}
	}
}

// setup is the developer's own command, so it records their environment as the relay's:
// a relay a hook starts later runs in it even when the hook's shell lacks TERMA_ENV.
func TestSetupRecordsTheRelayEnvironment(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	signInTo(t, gateway, orgA())
	if out, err := runTerma(t, "setup", "--harness", "codex"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	dir, err := daemon.Dir(testApp.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	env, ok := daemon.RecordedEnv(dir)
	if !ok {
		t.Fatal("setup recorded no relay environment")
	}
	for _, k := range []string{"TERMA_ENV", "TERMA_CONFIG_DIR", "HOME"} {
		if env[k] != os.Getenv(k) {
			t.Errorf("recorded %s=%q, setup ran with %q", k, env[k], os.Getenv(k))
		}
	}
}

// Antigravity exports nothing to the relay, but hooks run only while the relay's token
// exists: setup with it alone still mints the token, and writes agy's hooks.
func TestSetupWithAntigravityAloneTurnsHooksOn(t *testing.T) {
	if !testApp.agents.IsSupported("antigravity") {
		t.Skip("Antigravity is Coming Soon on Windows")
	}
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	signInTo(t, gateway, orgA())
	out, err := runTerma(t, "setup", "--harness", "antigravity")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if !claim.Enabled(testApp.stateDir) {
		t.Fatal("setup left no relay token, so every hook is off")
	}
	// The relay it starts reports this machine, as it does for an exporting agent.
	if !strings.Contains(out, "Check-in") {
		t.Errorf("setup did not check in:\n%s", out)
	}
	path := filepath.Join(os.Getenv("HOME"), ".gemini", "config", "hooks.json")
	hooks, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(hooks), "hook --user antigravity-stop") {
		t.Fatalf("agy hooks = %s, %v", hooks, err)
	}
	if out, _ := runTerma(t, "doctor"); strings.Contains(out, "switched off or out of date") {
		t.Errorf("doctor faults hooks that are in place:\n%s", out)
	}

	// Its hooks are all it reports through: doctor says when they are switched off, and
	// doctor and status when they are gone.
	disabled := strings.Replace(string(hooks), `"terma": {`, `"terma": {"enabled": false, `, 1)
	if err := os.WriteFile(path, []byte(disabled), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runTerma(t, "doctor"); err == nil || !strings.Contains(out, "switched off or out of date") {
		t.Errorf("doctor with terma's agy hook disabled: %v\n%s", err, out)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if out, err := runTerma(t, "doctor"); err == nil || !strings.Contains(out, "switched off or out of date") || !strings.Contains(out, "Antigravity") {
		t.Errorf("doctor with agy's hooks gone: %v\n%s", err, out)
	}
	if out, _ := runTerma(t, "status"); !strings.Contains(out, "switched off or out of date") {
		t.Errorf("status with agy's hooks gone:\n%s", out)
	}
}

// T3 Code runs its own Antigravity from a home of its own: setup writes terma's hook there
// too, beside the developer's other named hooks, and names the file when asked to show what
// it wrote; teardown takes it out.
func TestSetupWritesAgyHooksIntoT3CodesHomes(t *testing.T) {
	if !testApp.agents.IsSupported("antigravity") {
		t.Skip("Antigravity is Coming Soon on Windows")
	}
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	signInTo(t, gateway, orgA())
	t3 := filepath.Join(os.Getenv("T3CODE_HOME"), "userdata", "providers", "antigravity", "a1")
	if err := os.MkdirAll(filepath.Join(t3, "antigravity-acp"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t3, "config", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	const guard = `{"fleet-secret-guard": {"PreToolUse": [{"matcher": "", "hooks": [{"type": "command", "command": "/opt/fleet/guard"}]}]}}`
	if err := os.WriteFile(path, []byte(guard), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "setup", "--harness", "antigravity", "--verbose")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	hooks, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(hooks), "hook --user antigravity-stop") || !strings.Contains(string(hooks), "/opt/fleet/guard") {
		t.Fatalf("T3 Code's agy hooks = %s, %v", hooks, err)
	}
	if !strings.Contains(out, filepath.Join("antigravity", "a1", "config", "hooks.json")) {
		t.Errorf("setup did not name T3 Code's hooks file:\n%s", out)
	}
	if gemini, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".gemini", "config", "hooks.json")); err != nil || !strings.Contains(string(gemini), "hook --user antigravity-stop") {
		t.Fatalf("agy's own hooks = %s, %v", gemini, err)
	}
	if out, err := runTerma(t, "teardown", "--yes"); err != nil {
		t.Fatalf("teardown: %v\n%s", err, out)
	}
	hooks, err = os.ReadFile(path)
	if err != nil || strings.Contains(string(hooks), "hook --user") || !strings.Contains(string(hooks), "/opt/fleet/guard") {
		t.Fatalf("after teardown, T3 Code's agy hooks = %s, %v", hooks, err)
	}
}

// An agy on PATH too old for terma's hooks holds back only ~/.gemini, the home it uses:
// setup leaves that out, says why, and still writes T3 Code's home, which runs T3 Code's
// own agy.
func TestSetupWithAnOldAgyWritesT3CodesHomes(t *testing.T) {
	if !testApp.agents.IsSupported("antigravity") || runtime.GOOS == "windows" {
		t.Skip("Antigravity is Coming Soon on Windows")
	}
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	signInTo(t, gateway, orgA())
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "agy"), []byte("#!/bin/sh\necho 1.1.9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t3 := filepath.Join(os.Getenv("T3CODE_HOME"), "userdata", "providers", "antigravity", "a1")
	if err := os.MkdirAll(filepath.Join(t3, "antigravity-acp"), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "setup", "--harness", "antigravity", "--yes", "--verbose")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if hooks, err := os.ReadFile(filepath.Join(t3, "config", "hooks.json")); err != nil || !strings.Contains(string(hooks), "hook --user antigravity-stop") {
		t.Fatalf("T3 Code's agy hooks = %s, %v", hooks, err)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".gemini", "config", "hooks.json")); !os.IsNotExist(err) {
		t.Errorf("setup wrote ~/.gemini's hooks for an old agy: %v", err)
	}
	if !strings.Contains(out, "left ~/.gemini out") || !strings.Contains(out, "1.2.4 or later") {
		t.Errorf("setup did not say why it left ~/.gemini out:\n%s", out)
	}
}

// A ~/.gemini/config/hooks.json that does not parse, as agy's comments and trailing commas
// do not, holds up neither setup for another agent nor teardown, which still retires the
// relay's token.
func TestSetupAndTeardownPassAMalformedAgyHooksFile(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	signInTo(t, gateway, orgA())
	path := filepath.Join(os.Getenv("HOME"), ".gemini", "config", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	const mine = "// the developer's\n{\"lint\": {\"Stop\": [],},}\n"
	if err := os.WriteFile(path, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runTerma(t, "setup", "--harness", "claude"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if out, err := runTerma(t, "teardown", "--yes"); err != nil {
		t.Fatalf("teardown: %v\n%s", err, out)
	}
	if claim.Enabled(testApp.stateDir) {
		t.Error("teardown left the relay's token")
	}
	if got, _ := os.ReadFile(path); string(got) != mine {
		t.Errorf("hooks.json changed:\n%s", got)
	}
}

// Setup leaves an agent it was not asked for and never configured alone, even when its config does not parse.
func TestSetupLeavesAnUnconfiguredAgentsConfigAlone(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	signInTo(t, gateway, orgA())
	codexConfig := filepath.Join(os.Getenv("CODEX_HOME"), "config.toml")
	mine := "model = \"gpt-6\"\n[otel\n"
	if err := os.WriteFile(codexConfig, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runTerma(t, "setup", "--harness", "claude"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	token, err := daemon.Token(testApp.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json")); !strings.Contains(string(data), token) {
		t.Fatalf("Claude Code is not on the relay:\n%s", data)
	}
	if data, _ := os.ReadFile(codexConfig); string(data) != mine {
		t.Fatalf("Codex's config changed:\n%s", data)
	}
}

// An agent dropped at a later setup stops sending to the relay, while the agent still chosen keeps it.
func TestSetupReleasesTheRelayFromADeselectedAgent(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	signInTo(t, gateway, orgA())
	codexConfig := filepath.Join(codexHome, "config.toml")
	mine := "model = \"gpt-6\"\n\n[otel]\nexporter = { otlp-http = { endpoint = \"https://otel.example.com/v1/logs\", protocol = \"binary\" } }\n\n[otel.tool_result]\nmax_bytes = 4096\n"
	if err := os.WriteFile(codexConfig, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	// An agent terma never configured, whose own settings terma cannot parse, is no obstacle.
	t.Setenv("GEMINI_CLI_HOME", t.TempDir())
	geminiSettings := filepath.Join(os.Getenv("GEMINI_CLI_HOME"), ".gemini", "settings.json")
	geminiMine := "// mine\n{}\n"
	if err := os.MkdirAll(filepath.Dir(geminiSettings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(geminiSettings, []byte(geminiMine), 0o600); err != nil {
		t.Fatal(err)
	}
	claudeSettings := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json")
	read := func(path string) string {
		t.Helper()
		data, _ := os.ReadFile(path)
		return string(data)
	}

	if out, err := runTerma(t, "setup", "--harness", "claude,codex"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	token, err := daemon.Token(testApp.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(codexConfig), token) || !strings.Contains(read(claudeSettings), token) {
		t.Fatalf("setup did not point both agents at the relay:\ncodex:\n%s\nclaude:\n%s", read(codexConfig), read(claudeSettings))
	}

	out, err := runTerma(t, "setup", "--harness", "claude", "--verbose")
	if err != nil {
		t.Fatalf("setup without Codex: %v\n%s", err, out)
	}
	if got := read(codexConfig); strings.Contains(got, token) || !strings.Contains(got, "otel.example.com") || !strings.Contains(got, "max_bytes = 4096") {
		t.Fatalf("Codex's own [otel] is not back:\n%s", got)
	}
	if !strings.Contains(out, "Codex") || !strings.Contains(out, "no longer sends to the local relay") {
		t.Errorf("setup did not say Codex stopped sending:\n%s", out)
	}
	if !strings.Contains(read(claudeSettings), token) {
		t.Fatalf("Claude Code, still chosen, left the relay:\n%s", read(claudeSettings))
	}

	// And the other way round: dropping Claude Code takes its env block off the relay.
	if out, err := runTerma(t, "setup", "--harness", "codex"); err != nil {
		t.Fatalf("setup without Claude Code: %v\n%s", err, out)
	}
	if got := read(claudeSettings); strings.Contains(got, token) {
		t.Fatalf("Claude Code still sends to the relay:\n%s", got)
	}
	if !strings.Contains(read(codexConfig), token) {
		t.Fatalf("Codex, chosen again, is not on the relay:\n%s", read(codexConfig))
	}

	if out, err := runTerma(t, "teardown", "--yes"); err != nil {
		t.Fatalf("teardown: %v\n%s", err, out)
	}
	if got := read(codexConfig); strings.Contains(got, token) {
		t.Fatalf("Codex still sends to the relay after teardown:\n%s", got)
	}
	if got := read(geminiSettings); got != geminiMine {
		t.Fatalf("Gemini CLI's own settings changed:\n%s", got)
	}
}

// otlpRequest is one request an ingest host received: the key it came with and its body.
type otlpRequest struct {
	auth, project string
	body          []byte
}

// recordingIngest is an ingest host that takes every request and keeps it.
type recordingIngest struct {
	*httptest.Server
	mu       sync.Mutex
	requests []otlpRequest
}

func newRecordingIngest(t *testing.T) *recordingIngest {
	t.Helper()
	h := &recordingIngest{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.requests = append(h.requests, otlpRequest{auth: r.Header.Get("Authorization"), project: r.Header.Get(relay.ProjectHeader), body: body})
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(h.Close)
	return h
}

// take returns the requests received since the last take.
func (h *recordingIngest) take() []otlpRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	got := h.requests
	h.requests = nil
	return got
}

// The cutover when Terma Desktop moves the CLI to another organization (the organization-
// model council's item 1a), on user tokens: one sign-in serves both organizations, so the
// switch exchanges one code. Work in flight keeps its team and that team's restrictions:
// every request names, in X-Mirador-Project, the team its events were queued for, never the
// new one, and a relay on the new organization never sends the claimed session under the
// new team's wider policy. A switch back collects for the old team under its own policy.
func TestDesktopGrantCutoverKeepsInFlightWorkWithItsTeam(t *testing.T) {
	gateway := grantSandbox(t)
	t.Setenv("TERMA_POLICY_STUB", "") // the real fetches, which the auth gateway scopes
	gateway.scopedPolicies = true
	teamA, personal := teamOrgTeam(), personalTeam()
	gateway.policyBodies = map[string]string{
		// The SHARED organization's team: one repository, no prompts, no tool content.
		teamA: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/acme/app"]},"git_hooks":false,"capture":{"exclude_prompts":true,"exclude_tool_content":true}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
		// Second organization's team has a wider capture policy (prompts + tool content).
		personal: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/beta/side"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
	}
	ingest := newRecordingIngest(t)
	t.Setenv("TERMA_OTLP_URL", ingest.URL)
	gateway.grant("to-team", deskUser)
	setupFor := func(org organization, team, code string) {
		t.Helper()
		stdin := identityLine(deskUser, org.ID)
		if code != "" {
			stdin += `{"code":"` + code + `"}` + "\n"
		}
		if stdout, stderr, err := desktopSetup(t, stdin, org.ID, team); err != nil {
			t.Fatalf("setup for %s: %v\n%s%s", org.Name, err, stdout, stderr)
		}
		ingest.take() // setup's own check-in is not the delivery under test
	}
	// resolve is what a relay started now does with the claim of session sid.
	resolve := func(sid string) (relay.Policy, error) {
		t.Helper()
		c, ok := claim.Read(testApp.stateDir, sid, time.Now())
		if !ok {
			t.Fatalf("session %s has no claim", sid)
		}
		cfg, err := testApp.loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		return testApp.relayDeps().Resolver(cfg)(c)
	}
	flush := func() []otlpRequest {
		t.Helper()
		if _, err := testApp.flushSpool(t.Context(), true, 0); err != nil {
			t.Fatal(err)
		}
		return ingest.take()
	}
	token := "Bearer " + fakeUserToken(deskUser)
	// Each request names, as its team, the one whose events it carries.
	checkTeams := func(sent []otlpRequest) {
		t.Helper()
		for _, r := range sent {
			if r.auth != token {
				t.Fatalf("a request went with %q, not the sign-in's token", r.auth)
			}
			for _, team := range []string{teamA, personal} {
				if bytes.Contains(r.body, []byte(team)) != (r.project == team) {
					t.Fatalf("a request naming team %s carried team %s's events: %v", r.project, team, bytes.Contains(r.body, []byte(team)))
				}
			}
		}
	}

	// On the SHARED organization: events queued for team A, and a session claimed for it.
	setupFor(orgA(), teamA, "to-team")
	s := spoolForTest(t)
	appendEvent(t, s, teamA, time.Now())
	appendEvent(t, s, teamA, time.Now())
	signedIn, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !claim.Write(testApp.stateDir, "in-flight", claim.Claim{ProjectID: teamA, Tool: "codex", Repository: appRepo, Account: signedIn.Account}, time.Now()) {
		t.Fatal("could not claim the session")
	}
	if pol, err := resolve("in-flight"); err != nil || pol.Key != "" || pol.IncludePrompts || pol.IncludeToolContent {
		t.Fatalf("before the switch the claim resolves to %+v, %v; want team A's policy and no key", pol, err)
	}

	// Onboarding moves the CLI to Personal: the same sign-in, no second code.
	setupFor(personalOrg(), personal, "")
	appendEvent(t, s, personal, time.Now()) // Personal's own, which must still go
	sent := flush()
	if !slices.ContainsFunc(sent, func(r otlpRequest) bool { return r.project == personal }) {
		t.Fatal("Personal's own event was not delivered: the pass proves nothing")
	}
	checkTeams(sent)
	// A relay started on Personal never sends the claimed session under Personal's wider
	// policy: at most under team A's own restrictions.
	if pol, err := resolve("in-flight"); err == nil && (pol.IncludePrompts || pol.IncludeToolContent || pol.Unadmitted) {
		t.Fatalf("on Personal the claim for team A resolves to %+v", pol)
	}

	// The desktop switches back: the stored sign-in serves, and team A collects under its own
	// policy.
	setupFor(orgA(), teamA, "")
	if gateway.exchanges.Load() != 1 {
		t.Fatalf("the switches exchanged %d codes; the one sign-in serves both organizations", gateway.exchanges.Load())
	}
	pol, err := resolve("in-flight")
	if err != nil || pol.Key != "" || pol.IncludePrompts || pol.IncludeToolContent || pol.Unadmitted || pol.Signals != nil {
		t.Fatalf("back on the SHARED organization the claim resolves to %+v, %v; want team A's restrictions", pol, err)
	}
	appendEvent(t, s, teamA, time.Now())
	sent = flush()
	if !slices.ContainsFunc(sent, func(r otlpRequest) bool { return r.project == teamA }) {
		t.Fatal("team A's events were not delivered after the switch back")
	}
	checkTeams(sent)
	if queued := queuedProjects(t); len(queued) != 0 {
		t.Fatalf("queue = %v after the switch back", queued)
	}
}

// agy reads its hooks file as JSONC. A comment or trailing comma the developer adds after
// setup holds up neither setup run again nor teardown, which retires the relay's token
// and leaves the comment in place.
func TestSetupAndTeardownPatchACommentedAgyHooksFile(t *testing.T) {
	if !testApp.agents.IsSupported("antigravity") {
		t.Skip("Antigravity is Coming Soon on Windows")
	}
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	signInTo(t, gateway, orgA())
	if out, err := runTerma(t, "setup", "--harness", "antigravity"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	path := filepath.Join(os.Getenv("HOME"), ".gemini", "config", "hooks.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	commented := "// my agy hooks\n" + strings.Replace(string(data), "\n}", ",\n}", 1)
	if err := os.WriteFile(path, []byte(commented), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runTerma(t, "setup", "--harness", "antigravity"); err != nil {
		t.Fatalf("setup again: %v\n%s", err, out)
	}
	if out, err := runTerma(t, "teardown", "--yes"); err != nil {
		t.Fatalf("teardown: %v\n%s", err, out)
	}
	if claim.Enabled(testApp.stateDir) {
		t.Error("teardown left the relay's token")
	}
	got, err := os.ReadFile(path)
	if err == nil && (strings.Contains(string(got), "hook --user") || !strings.Contains(string(got), "// my agy hooks")) {
		t.Errorf("after teardown:\n%s", got)
	}
}
