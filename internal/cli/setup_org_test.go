package cli

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// With no terminal to ask on, setup keeps the stored sign-in's organization, but never
// silently: it says the developer belongs to others and how to set one of them up.
func TestSetupWithoutATerminalSaysItKeptOneOfSeveralOrganizations(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	for _, o := range []organization{orgB(), orgA()} {
		if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(gateway, o)); err != nil {
			t.Fatal(err)
		}
	}
	out, err := runTerma(t, "setup", "--harness", "codex")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	want := "! Organization  kept Acme, one of your 2: `terma setup --org <name>` adds another"
	if !strings.Contains(out, want) {
		t.Fatalf("setup did not say it kept one of several organizations (%d listings):\n%s", gateway.orgLists.Load(), out)
	}
	if active, _ := auth.LoadCredential(testApp.dir, config.DefaultProfile); active == nil || active.OrganizationID != orgA().ID {
		t.Fatalf("Acme should still be active: %+v", active)
	}
}

// setup asks which organization only when the stored sign-in's user belongs to several and
// --org named none; the answer, or the current one, is what stays signed in.
func TestSetupSignInChoosesAmongSeveralOrganizations(t *testing.T) {
	pickB := func(orgs []organization, _ string) (*organization, error) { return &orgs[1], nil }
	keep := func(orgs []organization, current string) (*organization, error) {
		return matchOrganization(orgs, current)
	}
	cancel := func([]organization, string) (*organization, error) { return nil, errCancelled }
	for _, tc := range []struct {
		name       string
		orgs       []organization
		org        string
		ask        orgAsker
		wantAsked  bool
		wantActive organization
		wantKept   int
		wantErr    error
	}{
		{name: "several, picks another", ask: pickB, wantAsked: true, wantActive: orgB()},
		{name: "several, keeps the current", ask: keep, wantAsked: true, wantActive: orgA()},
		{name: "several, cancelled", ask: cancel, wantAsked: true, wantErr: errCancelled},
		{name: "several, no terminal", wantActive: orgA(), wantKept: 2},
		{name: "only one", orgs: []organization{orgA()}, ask: pickB, wantActive: orgA()},
		{name: "--org names one", org: "beta", ask: pickB, wantActive: orgB()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateway := newFakeAuth(t)
			gateway.orgs = tc.orgs
			authSandbox(t, gateway)
			for _, o := range []organization{orgB(), orgA()} {
				if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(gateway, o)); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := testApp.loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			asked := false
			var ask orgAsker
			if tc.ask != nil {
				ask = func(orgs []organization, current string) (*organization, error) {
					asked = true
					if current != orgA().ID || len(orgs) != 2 {
						t.Errorf("asked with current %q among %d organizations", current, len(orgs))
					}
					return tc.ask(orgs, current)
				}
			}
			cmd := &cobra.Command{}
			cmd.SetContext(t.Context())
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetIn(strings.NewReader(""))
			got, kept, err := testApp.setupSignIn(cmd, cfg, signInOptions{org: parseOrgRef(tc.org), noBrowser: true}, ask)
			if asked != tc.wantAsked {
				t.Errorf("asked = %v, want %v", asked, tc.wantAsked)
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if kept != tc.wantKept {
				t.Errorf("kept = %d, want %d", kept, tc.wantKept)
			}
			if got.OrganizationID != tc.wantActive.ID || got.OrganizationName != tc.wantActive.Name {
				t.Errorf("config organization = %s %q, want %s", got.OrganizationID, got.OrganizationName, tc.wantActive.Name)
			}
			if active, _ := auth.LoadCredential(testApp.dir, config.DefaultProfile); active == nil || active.OrganizationID != tc.wantActive.ID {
				t.Errorf("active credential %+v, want %s", active, tc.wantActive.Name)
			}
		})
	}
}

// A setup that stops at the organization step leaves the credential where it was stored,
// though the sign-in before it saved the credential under the storage being tried.
func TestSetupStoppedAtTheOrganizationStepKeepsTheCredentialWhereItWas(t *testing.T) {
	gateway := newFakeAuth(t)
	gateway.orgsDown = true
	authSandbox(t, gateway)
	sandboxMachine(t)
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(gateway, orgA())); err != nil {
		t.Fatal(err)
	}
	if auth.StoredInFile(testApp.dir, config.DefaultProfile) {
		t.Skip("no keychain to keep the credential in")
	}
	out, err := runTerma(t, "setup", "--harness", "codex", "--insecure-storage")
	if err == nil || !strings.Contains(err.Error(), "list your organizations") {
		t.Fatalf("setup should stop at the organization step: %v\n%s", err, out)
	}
	if config.InsecureStorage(testApp.dir) || auth.StoredInFile(testApp.dir, config.DefaultProfile) {
		t.Fatal("the credential stayed in plain text after setup stopped")
	}
}

// An install from before organizations were added, its one team recorded in Team alone,
// keeps collecting for that team when `terma setup --org` adds another organization: the
// machine then collects for both, each team's repositories reporting to that team.
func TestSetupAddsAnOrganizationToAnOlderInstall(t *testing.T) {
	gateway := newFakeAuth(t)
	gateway.policyBody = `{"policy":{"version":"1.0","terma":{"capture":{"exclude_prompts":false,"exclude_tool_content":false},` +
		`"per_repository":{"repositories":["github.com/beta/site"]}}},"revision":1,"updated_at":"2026-01-01T00:00:00Z"}`
	authSandbox(t, gateway)
	sandboxMachine(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TERMA_POLICY_STUB", "")
	for _, o := range []organization{orgB(), orgA()} {
		if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(gateway, o)); err != nil {
			t.Fatal(err)
		}
	}
	acmeWeb, betaCore := projectsIn(orgA().ID)[0], projectsIn(orgB().ID)[0]
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) {
		p.OrganizationID, p.OrganizationName, p.Team = orgA().ID, orgA().Name, acmeWeb.ID
	}); err != nil {
		t.Fatal(err)
	}
	if err := config.WritePolicy(testApp.stateDir, config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/acme/app"},
		IncludePrompts: true, IncludeToolContent: true, TeamID: acmeWeb.ID, TeamName: acmeWeb.Name, OrganizationID: orgA().ID, OrganizationName: orgA().Name,
		AuthURL: gateway.srv.URL, Revision: 1, FetchedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "setup", "--org", orgB().Name, "--harness", "codex")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if !strings.Contains(out, "✓ Also") || !strings.Contains(out, "(github.com/acme/app), with prompts and tool content for team Acme Web of Acme") {
		t.Fatalf("setup did not say it also collects for the first organization:\n%s", out)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OrganizationID != orgB().ID || cfg.Policy.TeamID != betaCore.ID || cfg.Teams[orgA().ID] != acmeWeb.ID || cfg.Teams[orgB().ID] != betaCore.ID {
		t.Fatalf("after adding an organization: org %s, team %s, teams %v", cfg.OrganizationID, cfg.Policy.TeamID, cfg.Teams)
	}
	coll := routing.Collection(cfg)
	if p, ok := coll.Admitting(config.Repository{Origin: "github.com/acme/app"}); !ok || p.TeamID != acmeWeb.ID {
		t.Fatalf("the first organization's repository is collected by %+v, %v", p, ok)
	}
	if p, ok := coll.Admitting(config.Repository{Origin: "github.com/beta/site"}); !ok || p.TeamID != betaCore.ID {
		t.Fatalf("the added organization's repository is collected by %+v, %v", p, ok)
	}
	if _, ok := coll.Admitting(config.Repository{Origin: "github.com/me/personal"}); ok {
		t.Fatal("a repository neither team lists is collected")
	}
}
