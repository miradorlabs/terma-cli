package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// doctor fails closed on missing, unreadable, and wrong-account routing.State.
func TestDoctorFailsClosedOnMissingCorruptAndWrongAccountRoutingState(t *testing.T) {
	f, _ := routedMachine(t, "alice", func(f *fakeAuth, s *routing.State) error {
		_, err := s.Select(stateTeam(f, orgA().ID, teamsAcmeWeb, false, "github.com/acme/web"))
		return err
	})
	statePath, err := routing.StatePath(testApp.stateDir, config.DefaultProfile)
	if err != nil {
		t.Fatal(err)
	}

	// Wrong account.
	bobAcct := auth.AccountKey(f.srv.URL, "bob")
	if _, err := routing.Replace(testApp.stateDir, config.DefaultProfile, nil, bobAcct, func(s *routing.State) error {
		_, err := s.Select(stateTeam(f, orgA().ID, teamsAcmeWeb, false, "github.com/acme/web"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "doctor")
	if err == nil || !strings.Contains(out, "belongs to another account") || !strings.Contains(out, "terma setup") {
		t.Fatalf("wrong-account doctor (%v):\n%s", err, out)
	}

	// Corrupt/unreadable.
	if err := os.WriteFile(statePath, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = runTerma(t, "doctor")
	if err == nil || !strings.Contains(out, "routing state cannot be read") {
		t.Fatalf("corrupt doctor (%v):\n%s", err, out)
	}

	// Missing.
	_ = os.Remove(statePath)
	out, err = runTerma(t, "doctor")
	if err == nil || !strings.Contains(out, "has no routing state") {
		t.Fatalf("missing doctor (%v):\n%s", err, out)
	}
}

// doctor reports OrgAccessLost refusal, SignInNeeded with single
// `terma setup` remediation, and #152 outbox/spool losses together.
func TestDoctorReportsTeamsHealthSignInAndLosses(t *testing.T) {
	f, acct := routedMachine(t, "alice", func(f *fakeAuth, s *routing.State) error {
		if _, err := s.Select(stateTeam(f, orgA().ID, teamsAcmeWeb, false, "github.com/acme/web")); err != nil {
			return err
		}
		_, err := s.Select(stateTeam(f, orgB().ID, betaCore, false, "github.com/beta/core"))
		return err
	})
	_ = f
	// Mark Acme Web Refused with OrgAccessLost.
	if _, err := routing.Update(testApp.stateDir, config.DefaultProfile, routing.Expect{Account: acct}, func(s *routing.State) error {
		s.Teams[0].Refusals = 2
		s.Teams[0].LastError = routing.OrgAccessLost
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	names := teamNames{
		Teams:   map[string]string{teamsAcmeWeb: "Acme Web", betaCore: "Beta Core"},
		Orgs:    map[string]string{orgA().ID: "Acme", orgB().ID: "Beta Labs"},
		changed: true,
	}
	names.save(testApp.stateDir, config.DefaultProfile)

	// Relay stats with a stale blocked "mixed:" key from an earlier build, and #152 spool loss.
	claimDir := claim.Dir(testApp.stateDir)
	if err := os.MkdirAll(claimDir, 0o700); err != nil {
		t.Fatal(err)
	}
	snapBytes, _ := json.Marshal(relay.Snapshot{Blocked: map[string]int{"mixed:sess-mixed": 4}})
	if err := os.WriteFile(filepath.Join(claimDir, daemon.StatsFile), snapBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	sp, err := spool.Open(filepath.Join(testApp.stateDir, spool.Dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := sp.CountLost("no_destination", 3); err != nil {
		t.Fatal(err)
	}

	out, err := runTerma(t, "doctor")
	if err == nil {
		t.Fatalf("expected doctor failure on refused team:\n%s", out)
	}
	for _, want := range []string{
		"no access to this organization, skipped",
		"3 with no routed destination",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output lacks %q:\n%s", want, out)
		}
	}
	for _, gone := range []string{"paused", "sess-mixed"} {
		if strings.Contains(out, gone) {
			t.Errorf("doctor output still mentions %q:\n%s", gone, out)
		}
	}

	// Switch both teams to SignInNeeded: doctor warns with single `terma setup` fix.
	if _, err := routing.Update(testApp.stateDir, config.DefaultProfile, routing.Expect{Account: acct}, func(s *routing.State) error {
		s.Teams[0].Refusals, s.Teams[0].LastError, s.Teams[0].SignInNeeded = 0, routing.SignIn, true
		s.Teams[1].SignInNeeded, s.Teams[1].LastError = true, routing.SignIn
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out, _ = runTerma(t, "doctor")
	if !strings.Contains(out, "sign in again to send") || !strings.Contains(out, "\n    terma setup\n") {
		t.Errorf("SignInNeeded doctor output lacks single terma setup remediation:\n%s", out)
	}
	if strings.Contains(out, "terma teams --org") {
		t.Errorf("SignInNeeded doctor output still has stale per-org sign-in instruction:\n%s", out)
	}
}
