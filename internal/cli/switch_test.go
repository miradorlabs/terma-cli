package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// switchSandbox is a machine signed in to both fake organizations, with active the
// selected one and team its selected team.
func switchSandbox(t *testing.T, active organization, team string) *fakeAuth {
	t.Helper()
	f := newFakeAuth(t)
	authSandbox(t, f)
	sandboxMachine(t)
	for _, o := range fakeOrgs {
		if o.ID != active.ID {
			if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f, o)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f, active)); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) {
		p.SelectOrganization(active.ID, active.Name)
		p.Team = team
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

// selected is the organization and team the profile records, and how many sign-ins are stored.
func selected(t *testing.T) (org, team string, signIns int) {
	t.Helper()
	file, err := config.LoadFile(testApp.dir)
	if err != nil {
		t.Fatal(err)
	}
	if p := file.Profiles[config.DefaultProfile]; p != nil {
		org, team = p.OrganizationID, p.Team
	}
	creds, _ := auth.Credentials(testApp.dir, config.DefaultProfile)
	return org, team, len(creds)
}

// switchWith runs switch with stdin as the answer to its question, under a deadline that
// cuts setup's browser handoff short.
func switchWith(t *testing.T, f switchFlags, stdin string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(stdin))
	err := testApp.runSwitch(cmd, f)
	return out.String(), err
}

const acmeWeb = "aaaaaaaa-0000-4000-8000-000000000001"

// switch signs out of every organization, forgets the organization and team, and hands
// over to setup, which signs in in the browser rather than reusing a stored sign-in.
func TestSwitchSignsOutThenRunsSetup(t *testing.T) {
	f := switchSandbox(t, orgA(), acmeWeb)
	out, err := switchWith(t, switchFlags{assumeYes: true, noBrowser: true}, "")
	if err == nil || !strings.Contains(out, "Open this URL") {
		t.Fatalf("setup's browser sign-in should have started (and the deadline cut it): %v\n%s", err, out)
	}
	if got := f.revokes.Load(); got != 2 {
		t.Errorf("revoked %d sessions, want both organizations'", got)
	}
	if org, team, signIns := selected(t); org != "" || team != "" || signIns != 0 {
		t.Errorf("after switch: organization %q team %q, %d sign-ins stored", org, team, signIns)
	}
	if !strings.Contains(out, "private window") {
		t.Errorf("switch did not say how to approve as another account:\n%s", out)
	}
}

// Declining the question changes nothing.
func TestSwitchDeclinedChangesNothing(t *testing.T) {
	f := switchSandbox(t, orgA(), acmeWeb)
	out, err := switchWith(t, switchFlags{}, "n\n")
	if err != nil || !strings.Contains(out, "Nothing was changed") {
		t.Fatalf("declined switch: %v\n%s", err, out)
	}
	if org, team, signIns := selected(t); org != orgA().ID || team != acmeWeb || signIns != 2 || f.revokes.Load() != 0 {
		t.Errorf("after a declined switch: organization %q team %q, %d sign-ins, %d revokes", org, team, signIns, f.revokes.Load())
	}
}

// What would stop switch halfway is refused before anything changes.
func TestSwitchRefusesBeforeChangingAnything(t *testing.T) {
	for _, tc := range []struct {
		name, env, value string
		want             error
	}{
		{name: "server key", env: "TERMA_API_KEY", value: testServerKey, want: errNoSessionWithAPIKey},
		{name: "organization override", env: "TERMA_ORGANIZATION_ID", value: orgB().ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := switchSandbox(t, orgA(), acmeWeb)
			t.Setenv(tc.env, tc.value)
			_, err := switchWith(t, switchFlags{assumeYes: true, noBrowser: true}, "")
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) || !strings.Contains(err.Error(), tc.env) {
				t.Fatalf("err = %v", err)
			}
			if org, team, signIns := selected(t); org != orgA().ID || team != acmeWeb || signIns != 2 || f.revokes.Load() != 0 {
				t.Errorf("after a refused switch: organization %q team %q, %d sign-ins, %d revokes", org, team, signIns, f.revokes.Load())
			}
		})
	}
}
