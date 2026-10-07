package cli

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
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
	want := "! Organization  kept Acme, one of your 2: `terma setup --org <name>` sets up another"
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
