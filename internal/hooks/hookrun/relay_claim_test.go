package hookrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Any hook of a session in an installed repository, not only its start, claims it for
// the developer's team and reports the claim so the caller can start the relay.
func TestHooksClaimSessionsForTheRelay(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	root := hookruntest.InitRepo(t)
	sp, _ := spool.Open(t.TempDir())
	claimed := 0
	// The hook runs from elsewhere; the claim names the checkout its payload is in.
	env := func(stdin string) Env {
		return Env{StateDir: stateDir, Now: time.Now(), Cwd: t.TempDir(), Stdin: strings.NewReader(stdin), Spool: sp, Policy: hookruntest.Admitting(root), Team: "project-a", OnClaim: func(got string) {
			if claimed++; got != root {
				t.Errorf("OnClaim(%q), want the checkout %q", got, root)
			}
		}}
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
		c, ok := claim.Read(stateDir, sid, time.Now())
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
	t.Parallel()
	configDir, stateDir := t.TempDir(), t.TempDir()
	root := hookruntest.InitRepo(t)
	expired := config.Policy{Mode: config.ModeRepo, Repositories: hookruntest.Admitting(root).Repositories, TeamID: "project-a", Revision: 1, FetchedAt: time.Now().Add(-config.MaxPolicyAge - time.Hour)}
	sp, _ := spool.Open(t.TempDir())
	claimed := false
	env := Env{ConfigDir: configDir, StateDir: stateDir, Now: time.Now(), Cwd: root, Spool: sp, Policy: expired, Team: "project-a", OnClaim: func(string) { claimed = true },
		Stdin: strings.NewReader(`{"session_id":"expired-session","cwd":"` + hookruntest.InJSON(root) + `","model":"m"}`)}
	if err := startSession(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if c, ok := claim.Read(stateDir, "expired-session", time.Now()); !ok || c.ProjectID != "project-a" || !claimed {
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
	t.Parallel()
	stateDir := t.TempDir()
	root := hookruntest.InitRepo(t)
	sp, _ := spool.Open(t.TempDir())
	for sid, team := range map[string]string{"backend-session": "team-backend", "billing-session": "team-billing"} {
		env := Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Spool: sp, Team: team, Policy: hookruntest.Admitting(root),
			Stdin: strings.NewReader(`{"session_id":"` + sid + `","cwd":"` + hookruntest.InJSON(root) + `","model":"m"}`)}
		if err := startSession(context.Background(), env); err != nil {
			t.Fatal(err)
		}
		if c, ok := claim.Read(stateDir, sid, time.Now()); !ok || c.ProjectID != team {
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

// No team from setup: nothing is claimed. Without the relay's token no hook runs at all
// (dispatch.Run).
func TestNoClaimWithoutATeam(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	root := hookruntest.InitRepo(t)
	env := Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Stdin: strings.NewReader(`{"session_id":"s1","cwd":"` + hookruntest.InJSON(root) + `"}`)}
	_ = startSession(context.Background(), env)
	if _, ok := claim.Read(stateDir, "s1", time.Now()); ok {
		t.Fatal("a developer without a team claimed a session")
	}
}

// Global mode claims every session for the selected team, regardless of remote or
// whether it is inside a repository. Repo mode requires a team from setup.
func TestGlobalModeClaimsEverySession(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	global := config.Policy{Mode: config.ModeGlobal, DefaultProjectID: "p-default"}
	known := hookruntest.InitRepo(t)
	if _, err := gitx.Git(context.Background(), known, "remote", "set-url", "origin", "git@github.com:org/app.git"); err != nil {
		t.Fatal(err)
	}
	unknown := hookruntest.InitRepo(t)
	scratch := t.TempDir()
	last := hookruntest.InitRepo(t)
	claimIn := func(dir, sid string, pol config.Policy) (claim.Claim, bool) {
		env := Env{StateDir: stateDir, Now: time.Now(), Cwd: dir, Stdin: strings.NewReader(""), Policy: pol}
		ClaimFromPayload(context.Background(), env, PayloadSession{ID: sid, Cwd: dir}, "claude-code")
		return claim.Read(stateDir, sid, time.Now())
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
	// Outside git the claim names no working tree, but still the directory the hook ran in.
	want, _ := filepath.EvalSymlinks(scratch)
	if c, _ := claim.Read(stateDir, "g-scratch", time.Now()); c.Root != "" || c.Cwd != want {
		t.Errorf("scratch claim root %q, cwd %q; want no root and %q", c.Root, c.Cwd, want)
	}
}

// A claim names the directory its hook ran in next to the working tree: a session in a
// subdirectory resolves a tool call's relative paths and bare git commands there, not at the
// checkout's root. A spooled event and a bare payload claim alike.
func TestTheClaimNamesTheDirectoryTheHookRanIn(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	repo := hookruntest.InitRepo(t)
	sub := filepath.Join(repo, "frontend")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	root, _ := filepath.EvalSymlinks(repo)
	dir, _ := filepath.EvalSymlinks(sub)
	sp, _ := spool.Open(t.TempDir())
	env := Env{StateDir: stateDir, Now: time.Now(), Cwd: sub, Policy: hookruntest.Admitting(repo), Spool: sp, Version: "test", Team: "proj",
		Stdin: strings.NewReader(`{"session_id":"sess-sub","cwd":"` + hookruntest.InJSON(sub) + `","hook_event_name":"SessionStart"}`)}
	if err := startSession(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	payloadEnv := Env{StateDir: stateDir, Now: time.Now(), Cwd: sub, Policy: hookruntest.Admitting(repo), Team: "proj"}
	ClaimFromPayload(context.Background(), payloadEnv, PayloadSession{ID: "sess-payload", Cwd: sub}, "codex")
	for _, sid := range []string{"sess-sub", "sess-payload"} {
		if c, ok := claim.Read(stateDir, sid, time.Now()); !ok || c.Root != root || c.Cwd != dir {
			t.Errorf("%s: claim root %q, cwd %q, %v; want %q, %q", sid, c.Root, c.Cwd, ok, root, dir)
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
