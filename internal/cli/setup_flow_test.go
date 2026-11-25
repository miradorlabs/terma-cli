package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
)

type recordingSpinner struct {
	mu     sync.Mutex
	events *[]string
	active bool
}

func (s *recordingSpinner) Start(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		*s.events = append(*s.events, "spinner:update:"+text)
	} else {
		s.active = true
		*s.events = append(*s.events, "spinner:start:"+text)
	}
}

func (s *recordingSpinner) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return
	}
	s.active = false
	*s.events = append(*s.events, "spinner:stop")
}

// Verifies the streamlined setup order:
// 1. Auth -> 2. Fetch orgs (spinner on) -> 3. Fetch teams (same spinner continues) ->
// 4. Stop spinner -> 5. Harness picker -> 6. Automatically enroll all teams & bind repos
// without any org, team, or repository confirmation prompts.
func TestSetupStreamlinedOrderAndSpinner(t *testing.T) {
	gateway := newFakeAuth(t)
	acmeWeb := projectsIn(orgA().ID)[0].ID
	acmeAPI := projectsIn(orgA().ID)[1].ID
	betaTeam := projectsIn(orgB().ID)[0].ID
	gateway.policyBodies = map[string]string{
		acmeWeb:  `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/acme/web"]},"git_hooks":true,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
		acmeAPI:  `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/acme/api"]},"git_hooks":true,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
		betaTeam: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/beta/core"]},"git_hooks":true,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
	}
	authSandbox(t, gateway)
	sandboxMachine(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TERMA_POLICY_STUB", "")
	if err := saveSignIn(testApp.dir, storedSession(gateway)); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var events []string
	record := func(e string) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}

	prevSpin, prevAsk, prevForm, prevPick := testApp.newSpinner, testApp.canAsk, testApp.runForm, testApp.pickOne
	t.Cleanup(func() {
		testApp.newSpinner, testApp.canAsk, testApp.runForm, testApp.pickOne = prevSpin, prevAsk, prevForm, prevPick
	})
	testApp.newSpinner = func(io.Writer) progressSpinner {
		return &recordingSpinner{events: &events}
	}
	testApp.canAsk = func() bool { return true }
	testApp.pickOne = func(*cobra.Command, string, []pickRow, func(string) (int, error)) (int, error) {
		t.Fatal("setup must not invoke any org, team, or repository picker")
		return 0, nil
	}
	formsOpened := 0
	testApp.runForm = func(form *prompt.Form) ([]prompt.Item, error) {
		formsOpened++
		record("form:" + form.Title)
		items := slices.Clone(form.Items)
		for i := range items {
			items[i].Selected = strings.Contains(items[i].Label, "Codex")
		}
		return items, nil
	}

	out, err := runTerma(t, "setup")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if formsOpened != 1 {
		t.Fatalf("formsOpened = %d, want 1 (only harness picker)", formsOpened)
	}
	wantEvents := []string{
		"spinner:start:Fetching organizations…",
		"spinner:update:Fetching teams…",
		"spinner:stop",
		"form:Which coding agents do you use? (space toggles, enter confirms)",
	}
	if !slices.Equal(events, wantEvents) {
		t.Fatalf("events = %v\nwant   = %v", events, wantEvents)
	}
	// Org list fetched once and policies fetched once per team (no redundant fetches during SetupTeams).
	if n := gateway.orgLists.Load(); n != 1 {
		t.Fatalf("orgLists = %d, want 1", n)
	}
	if n := gateway.policies.Load(); n != 3 {
		t.Fatalf("policy fetches = %d, want 3 (one per team)", n)
	}

	st, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found {
		t.Fatalf("routing.Load = %v, found=%v", err, found)
	}
	if len(st.Teams) != 3 {
		t.Fatalf("enrolled %d teams, want 3: %+v", len(st.Teams), st.Teams)
	}
	wantBindings := map[string]string{
		"github.com/acme/web":  orgA().ID,
		"github.com/acme/api":  orgA().ID,
		"github.com/beta/core": orgB().ID,
	}
	for repo, wantOrg := range wantBindings {
		if got := st.Bindings[repo]; got.Organization != wantOrg || got.Refused {
			t.Errorf("binding[%s] = %+v, want org %s", repo, got, wantOrg)
		}
	}
	if strings.Contains(out, "Conflict") {
		t.Errorf("unexpected conflict output for disjoint repos:\n%s", out)
	}
}

func TestSetupCancelAtHarnessPickerCommitsNothing(t *testing.T) {
	gateway := newFakeAuth(t)
	gateway.policyBodies = map[string]string{
		projectsIn(orgA().ID)[0].ID: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/acme/web"]},"git_hooks":true,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
		projectsIn(orgA().ID)[1].ID: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/acme/api"]},"git_hooks":true,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
		projectsIn(orgB().ID)[0].ID: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/beta/core"]},"git_hooks":true,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
	}
	authSandbox(t, gateway)
	sandboxMachine(t)
	t.Setenv("TERMA_POLICY_STUB", "")
	if err := saveSignIn(testApp.dir, storedSession(gateway)); err != nil {
		t.Fatal(err)
	}

	prevAsk, prevForm := testApp.canAsk, testApp.runForm
	t.Cleanup(func() { testApp.canAsk, testApp.runForm = prevAsk, prevForm })
	testApp.canAsk = func() bool { return true }
	testApp.runForm = func(*prompt.Form) ([]prompt.Item, error) { return nil, prompt.ErrCancelled }

	out, err := runTerma(t, "setup")
	if err != nil {
		t.Fatalf("cancelled setup returned error: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Cancelled. Nothing was recorded.") {
		t.Fatalf("unexpected cancellation output:\n%s", out)
	}
	if _, found, err := routing.Load(testApp.stateDir, config.DefaultProfile); err != nil || found {
		t.Fatalf("routing state should not be written when harness picker is cancelled (found=%v, err=%v)", found, err)
	}
	if cfg, err := testApp.loadConfig(); err != nil || len(cfg.Harnesses) != 0 {
		t.Fatalf("harnesses should remain empty on cancellation: %+v, %v", cfg.Harnesses, err)
	}
}

func TestSetupPolicyConflictsNotified(t *testing.T) {
	policyJSON := func(version string, repos ...string) string {
		quoted := make([]string, len(repos))
		for i, r := range repos {
			quoted[i] = fmt.Sprintf("%q", r)
		}
		return fmt.Sprintf(`{"policy":{"version":%q,"terma":{"per_repository":{"repositories":[%s]},"git_hooks":true,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
			version, strings.Join(quoted, ","))
	}

	acmeWeb := projectsIn(orgA().ID)[0].ID
	acmeAPI := projectsIn(orgA().ID)[1].ID
	betaCoreID := projectsIn(orgB().ID)[0].ID

	t.Run("cross-org exact repo overlap left pending and notified while disjoint repos auto-bind", func(t *testing.T) {
		gateway := newFakeAuth(t)
		gateway.policyBodies = map[string]string{
			acmeWeb:    policyJSON("1.1", "github.com/acme/web", "github.com/shared/app"),
			acmeAPI:    policyJSON("1.1", "github.com/acme/api"),
			betaCoreID: policyJSON("1.1", "github.com/beta/core", "github.com/shared/app"),
		}
		authSandbox(t, gateway)
		sandboxMachine(t)
		t.Setenv("CODEX_HOME", t.TempDir())
		t.Setenv("TERMA_POLICY_STUB", "")
		if err := saveSignIn(testApp.dir, storedSession(gateway)); err != nil {
			t.Fatal(err)
		}

		out, err := runTerma(t, "setup", "--harness", "codex")
		if err != nil {
			t.Fatalf("setup: %v\n%s", err, out)
		}
		for _, want := range []string{
			"! Conflict",
			"github.com/shared/app is claimed by multiple organizations (Acme, Beta Labs); left pending",
			"terma teams --bind github.com/shared/app=<org>",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "Collects") && strings.Contains(line, "github.com/shared/app") {
				t.Errorf("Collects line must not report pending conflicting repo:\n%s", line)
			}
		}
		st, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
		if err != nil || !found {
			t.Fatalf("routing.Load = %v, found=%v", err, found)
		}
		if _, bound := st.Bindings["github.com/shared/app"]; bound {
			t.Fatalf("conflicting repo must remain pending, got %+v", st.Bindings["github.com/shared/app"])
		}
		if st.Bindings["github.com/acme/web"].Organization != orgA().ID || st.Bindings["github.com/beta/core"].Organization != orgB().ID {
			t.Fatalf("non-conflicting repos should still auto-bind: %+v", st.Bindings)
		}
	})

	t.Run("cross-org wildcard vs exact repo left pending and notified", func(t *testing.T) {
		gateway := newFakeAuth(t)
		gateway.policyBodies = map[string]string{
			acmeWeb:    policyJSON("2.0", "github.com/shared/*"),
			acmeAPI:    policyJSON("1.1", "github.com/acme/api"),
			betaCoreID: policyJSON("1.1", "github.com/shared/web"),
		}
		authSandbox(t, gateway)
		sandboxMachine(t)
		t.Setenv("CODEX_HOME", t.TempDir())
		t.Setenv("TERMA_POLICY_STUB", "")
		if err := saveSignIn(testApp.dir, storedSession(gateway)); err != nil {
			t.Fatal(err)
		}

		out, err := runTerma(t, "setup", "--harness", "codex")
		if err != nil {
			t.Fatalf("setup: %v\n%s", err, out)
		}
		if !strings.Contains(out, "! Conflict") || !strings.Contains(out, "github.com/shared/* (Acme) conflicts with github.com/shared/web (Beta Labs); left pending") {
			t.Fatalf("expected wildcard conflict notification:\n%s", out)
		}
		st, _, err := routing.Load(testApp.stateDir, config.DefaultProfile)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := st.Bindings["github.com/shared"]; ok {
			t.Fatalf("conflicting owner wildcard must stay pending: %+v", st.Bindings)
		}
		if _, ok := st.Bindings["github.com/shared/web"]; ok {
			t.Fatalf("conflicting exact repo under wildcard must stay pending: %+v", st.Bindings)
		}
		if st.Bindings["github.com/acme/api"].Organization != orgA().ID {
			t.Fatalf("unrelated repo should auto-bind: %+v", st.Bindings)
		}
	})

	t.Run("disjoint exact repos under same owner across different orgs auto-bind with no conflict", func(t *testing.T) {
		gateway := newFakeAuth(t)
		gateway.policyBodies = map[string]string{
			acmeWeb:    policyJSON("1.1", "github.com/shared/a"),
			acmeAPI:    policyJSON("1.1", "github.com/acme/api"),
			betaCoreID: policyJSON("1.1", "github.com/shared/b"),
		}
		authSandbox(t, gateway)
		sandboxMachine(t)
		t.Setenv("CODEX_HOME", t.TempDir())
		t.Setenv("TERMA_POLICY_STUB", "")
		if err := saveSignIn(testApp.dir, storedSession(gateway)); err != nil {
			t.Fatal(err)
		}

		out, err := runTerma(t, "setup", "--harness", "codex")
		if err != nil {
			t.Fatalf("setup: %v\n%s", err, out)
		}
		if strings.Contains(out, "Conflict") {
			t.Fatalf("unexpected conflict for disjoint repos under same owner:\n%s", out)
		}
		st, _, err := routing.Load(testApp.stateDir, config.DefaultProfile)
		if err != nil {
			t.Fatal(err)
		}
		if st.Bindings["github.com/shared/a"].Organization != orgA().ID || st.Bindings["github.com/shared/b"].Organization != orgB().ID {
			t.Fatalf("bindings = %+v, want both disjoint repos bound", st.Bindings)
		}
	})

	t.Run("same-org duplicate policy overlap binds to org and notifies routing precedence", func(t *testing.T) {
		gateway := newFakeAuth(t)
		gateway.orgs = []organization{orgA()}
		gateway.policyBodies = map[string]string{
			acmeWeb: policyJSON("1.1", "github.com/acme/shared"),
			acmeAPI: policyJSON("1.1", "github.com/acme/shared"),
		}
		authSandbox(t, gateway)
		sandboxMachine(t)
		t.Setenv("CODEX_HOME", t.TempDir())
		t.Setenv("TERMA_POLICY_STUB", "")
		if err := saveSignIn(testApp.dir, storedSession(gateway)); err != nil {
			t.Fatal(err)
		}

		out, err := runTerma(t, "setup", "--harness", "codex")
		if err != nil {
			t.Fatalf("setup: %v\n%s", err, out)
		}
		want := "github.com/acme/shared is listed by multiple teams in Acme (Acme Web, Acme API); sessions will route to Acme Web"
		if !strings.Contains(out, want) {
			t.Fatalf("missing same-org overlap notification %q:\n%s", want, out)
		}
		st, _, err := routing.Load(testApp.stateDir, config.DefaultProfile)
		if err != nil {
			t.Fatal(err)
		}
		if st.Bindings["github.com/acme/shared"].Organization != orgA().ID {
			t.Fatalf("same-org repo should still bind to Acme: %+v", st.Bindings)
		}
	})

	t.Run("existing refusal and org binding preserved and reported on re-run", func(t *testing.T) {
		gateway := newFakeAuth(t)
		gateway.policyBodies = map[string]string{
			acmeWeb:    policyJSON("1.1", "github.com/acme/web"),
			acmeAPI:    policyJSON("1.1", "github.com/acme/api"),
			betaCoreID: policyJSON("1.1", "github.com/beta/core"),
		}
		authSandbox(t, gateway)
		sandboxMachine(t)
		t.Setenv("CODEX_HOME", t.TempDir())
		t.Setenv("TERMA_POLICY_STUB", "")
		cred := storedSession(gateway)
		if err := saveSignIn(testApp.dir, cred); err != nil {
			t.Fatal(err)
		}
		if _, err := routing.Create(testApp.stateDir, config.DefaultProfile, cred.Account(), func(s *routing.State) error {
			if err := s.Refuse("github.com/acme/web"); err != nil {
				return err
			}
			return s.Bind("github.com/beta/core", orgA().ID, false)
		}); err != nil {
			t.Fatal(err)
		}

		out, err := runTerma(t, "setup", "--harness", "codex")
		if err != nil {
			t.Fatalf("setup: %v\n%s", err, out)
		}
		for _, want := range []string{
			"github.com/acme/web is refused on this machine",
			"github.com/beta/core is already bound to Acme",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing existing-decision conflict %q:\n%s", want, out)
			}
		}
		st, _, err := routing.Load(testApp.stateDir, config.DefaultProfile)
		if err != nil {
			t.Fatal(err)
		}
		if !st.Bindings["github.com/acme/web"].Refused {
			t.Fatalf("existing refusal was overwritten: %+v", st.Bindings["github.com/acme/web"])
		}
		if st.Bindings["github.com/beta/core"].Organization != orgA().ID {
			t.Fatalf("existing org binding was overwritten: %+v", st.Bindings["github.com/beta/core"])
		}
		if st.Bindings["github.com/acme/api"].Organization != orgA().ID {
			t.Fatalf("new repo should still auto-bind: %+v", st.Bindings["github.com/acme/api"])
		}
	})

	t.Run("inaccessible org reports warning without blocking other orgs", func(t *testing.T) {
		gateway := newFakeAuth(t)
		gateway.policyBodies = map[string]string{
			acmeWeb: policyJSON("1.1", "github.com/acme/web"),
			acmeAPI: policyJSON("1.1", "github.com/acme/api"),
		}
		gateway.orgGone = orgB().ID
		authSandbox(t, gateway)
		sandboxMachine(t)
		t.Setenv("CODEX_HOME", t.TempDir())
		t.Setenv("TERMA_POLICY_STUB", "")
		if err := saveSignIn(testApp.dir, storedSession(gateway)); err != nil {
			t.Fatal(err)
		}

		out, err := runTerma(t, "setup", "--harness", "codex")
		if err != nil {
			t.Fatalf("setup: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Beta Labs: "+orgInaccessibleNote) {
			t.Errorf("output missing inaccessible org notice:\n%s", out)
		}
		st, _, err := routing.Load(testApp.stateDir, config.DefaultProfile)
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Teams) != 2 || st.Bindings["github.com/acme/web"].Organization != orgA().ID {
			t.Fatalf("expected Acme teams enrolled despite inaccessible Beta Labs: teams=%+v bindings=%+v", st.Teams, st.Bindings)
		}
	})
}
