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
	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// relaySetUp writes the token `terma relay setup` leaves, which is what makes hooks
// claim sessions at all.
func relaySetUp(t *testing.T) {
	t.Helper()
	path, err := claim.TokenPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Any hook of a session in a bound repository claims it for the relay — Claude's and
// Codex's alike, and not only their session starts — and reports the claim so the
// caller can start the relay.
func TestHooksClaimSessionsForTheRelay(t *testing.T) {
	root := initRepo(t)
	relaySetUp(t)
	if err := project.Save(root, &project.File{Project: project.Project{ID: "project-a"}}); err != nil {
		t.Fatal(err)
	}
	sp, _ := spool.Open(t.TempDir())
	claimed := 0
	env := func(stdin string) Env {
		return Env{Now: time.Now(), Cwd: root, Stdin: strings.NewReader(stdin), Spool: sp, OnClaim: func() { claimed++ }}
	}
	const claude, codex = "claude-session-1", "codex-thread-1"
	if err := SessionStart(context.Background(), env(`{"session_id":"`+claude+`","cwd":"`+root+`","model":"m"}`)); err != nil {
		t.Fatal(err)
	}
	// A Codex session whose first hook is a tool call still claims.
	if err := CodexPostToolUse(context.Background(), env(`{"session_id":"`+codex+`","cwd":"`+root+`","tool_name":"shell","tool_input":{"command":"ls"}}`)); err != nil {
		t.Fatal(err)
	}
	// Nothing was spooled for that call, so the hook's payload claims it.
	payload := `{"session_id":"` + codex + `","cwd":"` + root + `","tool_name":"shell"}`
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

// No binding, or no relay on this machine: nothing is claimed.
func TestNoClaimWithoutBindingOrRelay(t *testing.T) {
	root := initRepo(t)
	env := Env{Now: time.Now(), Cwd: root, Stdin: strings.NewReader(`{"session_id":"s1","cwd":"` + root + `"}`)}
	relaySetUp(t)
	_ = SessionStart(context.Background(), env)
	if _, ok := claim.Read("s1", time.Now()); ok {
		t.Fatal("a repository without a binding claimed its session")
	}

	root = initRepo(t) // a fresh config dir: no relay token
	if err := project.Save(root, &project.File{Project: project.Project{ID: "project-a"}}); err != nil {
		t.Fatal(err)
	}
	env = Env{Now: time.Now(), Cwd: root, Stdin: strings.NewReader(`{"session_id":"s2","cwd":"` + root + `"}`)}
	_ = SessionStart(context.Background(), env)
	if _, ok := claim.Read("s2", time.Now()); ok {
		t.Fatal("a machine without `terma relay setup` claimed a session")
	}
}

// A Codex subagent exports under its own thread id, which its hooks name as agent_id:
// both ids are claimed, from a spooled event and from a bare payload alike.
func TestCodexSubagentThreadIsClaimed(t *testing.T) {
	root := initRepo(t)
	relaySetUp(t)
	if err := project.Save(root, &project.File{Project: project.Project{ID: "project-a"}}); err != nil {
		t.Fatal(err)
	}
	sp, _ := spool.Open(t.TempDir())
	env := Env{Now: time.Now(), Cwd: root, Spool: sp,
		Stdin: strings.NewReader(`{"session_id":"root-thread","agent_id":"child-thread","agent_type":"worker","cwd":"` + root + `"}`)}
	if err := CodexSubagentStart(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	payload := `{"session_id":"root-thread","agent_id":"child-thread-2","cwd":"` + root + `"}`
	ClaimFromPayload(context.Background(), Env{Now: time.Now(), Cwd: root}, mustPayloadSession(t, payload), "codex")
	for _, id := range []string{"root-thread", "child-thread", "child-thread-2"} {
		if c, ok := claim.Read(id, time.Now()); !ok || c.ProjectID != "project-a" {
			t.Errorf("%s not claimed: %+v %v", id, c, ok)
		}
	}
}

// Global mode claims every session for the selected team, regardless of remote,
// repository binding, or whether it is inside a repository. Repo mode requires a binding.
func TestGlobalModeClaimsEverySession(t *testing.T) {
	global := config.Policy{Mode: config.ModeGlobal, DefaultProjectID: "p-default"}
	known := initRepo(t)
	if _, err := gitx.Git(context.Background(), known, "remote", "add", "origin", "git@github.com:org/app.git"); err != nil {
		t.Fatal(err)
	}
	unknown := initRepo(t)
	scratch := t.TempDir()
	bound := initRepo(t) // each initRepo moves the config dir: the last one holds the claims
	relaySetUp(t)
	if err := project.Save(bound, &project.File{Project: project.Project{ID: "p-bound"}}); err != nil {
		t.Fatal(err)
	}
	claimIn := func(dir, sid string, pol config.Policy) (claim.Claim, bool) {
		env := Env{Now: time.Now(), Cwd: dir, Stdin: strings.NewReader(""), Policy: pol}
		ClaimFromPayload(context.Background(), env, PayloadSession{ID: sid, Cwd: dir}, "claude-code")
		return claim.Read(sid, time.Now())
	}
	for _, tc := range []struct{ dir, sid, want string }{
		{known, "g-known", "p-default"},
		{unknown, "g-unknown", "p-default"},
		{scratch, "g-scratch", "p-default"},
		{bound, "g-bound", "p-default"},
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
