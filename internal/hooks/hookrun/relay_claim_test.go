package hookrun

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Any hook of a session in an installed repository, not only its start, claims it for
// the developer's team and reports the claim so the caller can start the relay.
func TestHooksClaimSessionsForTheRelay(t *testing.T) {
	root := initRepo(t)
	hookruntest.RelayOn(t)
	sp, _ := spool.Open(t.TempDir())
	claimed := 0
	env := func(stdin string) Env {
		return Env{Now: time.Now(), Cwd: root, Stdin: strings.NewReader(stdin), Spool: sp, Policy: hookruntest.Admitting(root), Team: "project-a", OnClaim: func() { claimed++ }}
	}
	const claude, codex = "claude-session-1", "codex-thread-1"
	if err := startSession(context.Background(), env(`{"session_id":"`+claude+`","cwd":"`+hookruntest.InJSON(root)+`","model":"m"}`)); err != nil {
		t.Fatal(err)
	}
	// A session whose first hook is a tool call still claims.
	if err := editFile(context.Background(), env(`{"session_id":"`+codex+`","cwd":"`+hookruntest.InJSON(root)+`","tool_name":"shell","tool_input":{"command":"ls"}}`)); err != nil {
		t.Fatal(err)
	}
	// Nothing was spooled for that call, so the hook's payload claims it.
	payload := `{"session_id":"` + codex + `","cwd":"` + hookruntest.InJSON(root) + `","tool_name":"shell"}`
	if !ClaimFromPayload(context.Background(), env(""), mustPayloadSession(t, payload), "codex") {
		t.Fatal("a Codex hook that spooled nothing did not claim from its payload")
	}
	for sid, tool := range map[string]string{claude: "claude-code", codex: "codex"} {
		c, ok := claim.Read(sid, time.Now())
		if !ok || c.ProjectID != "project-a" || c.Tool != tool || c.Repo != filepath.Base(root) {
			t.Errorf("%s claim = %+v, %v", sid, c, ok)
		}
	}
	if claimed == 0 {
		t.Fatal("OnClaim was never called")
	}
}

// An expired policy collects nothing, but a hook still claims its session for the team
// and stamps its events with it, since the claim is what starts the relay that refreshes
// the policy, and delivery holds the events to the policy in force when they leave.
func TestAnExpiredPolicyStillClaims(t *testing.T) {
	root := initRepo(t)
	hookruntest.RelayOn(t)
	expired := config.Policy{Mode: config.ModeRepo, Folders: []string{filepath.Base(root)}, TeamID: "project-a", Revision: 1, FetchedAt: time.Now().Add(-config.MaxPolicyAge - time.Hour)}
	if err := routing.SavePolicy(expired); err != nil {
		t.Fatal(err)
	}
	sp, _ := spool.Open(t.TempDir())
	claimed := false
	env := Env{Now: time.Now(), Cwd: root, Spool: sp, Policy: expired, Team: "project-a", OnClaim: func() { claimed = true },
		Stdin: strings.NewReader(`{"session_id":"expired-session","cwd":"` + hookruntest.InJSON(root) + `","model":"m"}`)}
	if err := startSession(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if c, ok := claim.Read("expired-session", time.Now()); !ok || c.ProjectID != "project-a" || !claimed {
		t.Fatalf("claim = %+v, %v; OnClaim called = %v", c, ok, claimed)
	}
	for _, e := range hookruntest.Spooled(t, sp) {
		if e.Attrs[AttrProjectID] != "project-a" {
			t.Fatalf("%s spooled without the team: %v", e.Name, e.Attrs)
		}
	}
}

// Two developers on different teams in one repository each claim their sessions, and
// stamp their events, for their own team.
func TestEachDeveloperClaimsForTheirOwnTeam(t *testing.T) {
	root := initRepo(t)
	hookruntest.RelayOn(t)
	sp, _ := spool.Open(t.TempDir())
	for sid, team := range map[string]string{"backend-session": "team-backend", "billing-session": "team-billing"} {
		env := Env{Now: time.Now(), Cwd: root, Spool: sp, Team: team, Policy: hookruntest.Admitting(root),
			Stdin: strings.NewReader(`{"session_id":"` + sid + `","cwd":"` + hookruntest.InJSON(root) + `","model":"m"}`)}
		if err := startSession(context.Background(), env); err != nil {
			t.Fatal(err)
		}
		if c, ok := claim.Read(sid, time.Now()); !ok || c.ProjectID != team {
			t.Errorf("%s claim = %+v, %v; want %s", sid, c, ok, team)
		}
	}
	for _, e := range hookruntest.Spooled(t, sp) {
		want := map[string]string{"backend-session": "team-backend", "billing-session": "team-billing"}[e.SessionID]
		if e.Attrs[AttrProjectID] != want {
			t.Errorf("%s from %s: project %v, want %s", e.Name, e.SessionID, e.Attrs[AttrProjectID], want)
		}
	}
}

// No team from setup, or no relay on this machine: nothing is claimed.
func TestNoClaimWithoutATeamOrRelay(t *testing.T) {
	root := initRepo(t)
	env := Env{Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Stdin: strings.NewReader(`{"session_id":"s1","cwd":"` + hookruntest.InJSON(root) + `"}`)}
	hookruntest.RelayOn(t)
	_ = startSession(context.Background(), env)
	if _, ok := claim.Read("s1", time.Now()); ok {
		t.Fatal("a developer without a team claimed a session")
	}

	root = initRepo(t) // a fresh config dir: no relay token
	env = Env{Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Team: "project-a", Stdin: strings.NewReader(`{"session_id":"s2","cwd":"` + hookruntest.InJSON(root) + `"}`)}
	_ = startSession(context.Background(), env)
	if _, ok := claim.Read("s2", time.Now()); ok {
		t.Fatal("a machine without `terma relay setup` claimed a session")
	}
}

// Global mode claims every session for the selected team, regardless of remote or
// whether it is inside a repository. Repo mode requires a team from setup.
func TestGlobalModeClaimsEverySession(t *testing.T) {
	global := config.Policy{Mode: config.ModeGlobal, DefaultProjectID: "p-default"}
	known := initRepo(t)
	if _, err := gitx.Git(context.Background(), known, "remote", "add", "origin", "git@github.com:org/app.git"); err != nil {
		t.Fatal(err)
	}
	unknown := initRepo(t)
	scratch := t.TempDir()
	last := initRepo(t) // each initRepo moves the config dir: the last one holds the claims
	hookruntest.RelayOn(t)
	claimIn := func(dir, sid string, pol config.Policy) (claim.Claim, bool) {
		env := Env{Now: time.Now(), Cwd: dir, Stdin: strings.NewReader(""), Policy: pol}
		ClaimFromPayload(context.Background(), env, PayloadSession{ID: sid, Cwd: dir}, "claude-code")
		return claim.Read(sid, time.Now())
	}
	for _, tc := range []struct{ dir, sid, want string }{
		{known, "g-known", "p-default"},
		{unknown, "g-unknown", "p-default"},
		{scratch, "g-scratch", "p-default"},
		{last, "g-last", "p-default"},
	} {
		c, ok := claimIn(tc.dir, tc.sid, global)
		if !ok || c.ProjectID != tc.want {
			t.Errorf("%s: claim %+v, %v; want %s", tc.sid, c, ok, tc.want)
		}
	}
	for _, dir := range []string{known, unknown, scratch} {
		if _, ok := claimIn(dir, "r-"+filepath.Base(dir), config.DefaultPolicy()); ok {
			t.Errorf("repo mode claimed a session in %s", dir)
		}
	}
}

func mustPayloadSession(t *testing.T, payload string) PayloadSession {
	t.Helper()
	s, ok := ReadPayloadSession([]byte(payload))
	if !ok {
		t.Fatalf("no session in %s", payload)
	}
	return s
}
