package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// migrate marks testApp's machine migrated, as the migration's credential index does, and
// writes st, when given, as its profile's routing state.
func migrate(t *testing.T, st *routing.State) {
	t.Helper()
	if err := os.WriteFile(config.CredentialsV2Path(testApp.dir), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st != nil {
		hookruntest.WriteRoutingState(t, testApp.stateDir, st)
	}
}

// A machine not migrated hooks on its team and its policy, as before, with no routing.
func TestHooksOnAMachineNotMigratedReadTheTeam(t *testing.T) {
	root := collectedRepo(t)
	p := testApp.hookProfile()
	if p.Routing != nil || p.Team != "team" || !p.Policy.Admits(config.Repository{Origin: "github.com/acme/" + filepath.Base(root)}) {
		t.Fatalf("profile %+v: the team and its policy, no routing", p)
	}
}

// A migrated machine hooks on the profile's routing state, never on a team or a policy,
// even one config.json still names; with the routing state missing they route nothing.
func TestHooksOnAMigratedMachineReadTheRoutingState(t *testing.T) {
	collectedRepo(t)
	migrate(t, nil)
	if p := testApp.hookProfile(); p.Routing == nil || p.Routing.Usable() || p.Team != "" {
		t.Fatalf("profile %+v with the routing state missing: routing that routes nothing", p)
	}
	hookruntest.WriteRoutingState(t, testApp.stateDir, hookruntest.TwoOrganizations(t))
	p := testApp.hookProfile()
	if p.Routing == nil || !p.Routing.Usable() || p.Routing.Profile != config.DefaultProfile || p.Team != "" || p.Policy.Admits(config.Repository{Origin: "github.com/acme/x"}) {
		t.Fatalf("profile %+v: the profile's routing, and no team or policy", p)
	}
}

// End to end on a migrated machine: an agent session in a repository routing sends to a
// team with git_hooks on installs the repository's commit hooks and has its commit stamped;
// with git_hooks off neither happens. Every spooled event names its destination.
func TestAMigratedMachinesCommitHooksFollowRouting(t *testing.T) {
	for _, gitHooks := range []bool{true, false} {
		t.Run(map[bool]string{true: "git_hooks on", false: "git_hooks off"}[gitHooks], func(t *testing.T) {
			root := collectedRepo(t)
			t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			prev := testApp.hookExecutable
			testApp.hookExecutable = func() (string, error) { return "/opt/terma/bin/terma", nil }
			t.Cleanup(func() { testApp.hookExecutable = prev })
			st := hookruntest.RoutingState(t, []routing.Team{hookruntest.RoutedTeam("acme", "acme-platform", gitHooks, "github.com/acme/*")},
				map[string]string{"github.com/acme": "acme"})
			migrate(t, st)

			const sid = "sess-routed"
			runHook(t, "session-start", `{"session_id":"`+sid+`","cwd":"`+hookruntest.InJSON(root)+`","hook_event_name":"SessionStart","source":"startup"}`)
			if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			runHook(t, "post-tool-use", `{"session_id":"`+sid+`","cwd":"`+hookruntest.InJSON(root)+`","hook_event_name":"PostToolUse","tool_name":"Write","tool_input":{"file_path":"`+
				hookruntest.InJSON(filepath.Join(root, "a.txt"))+`"}}`)
			if _, err := os.Stat(filepath.Join(root, ".git", "hooks", "prepare-commit-msg")); (err == nil) != gitHooks {
				t.Errorf("commit hooks installed: %v, want %v", err == nil, gitHooks)
			}
			if out, err := exec.Command("git", "-C", root, "add", "a.txt").CombinedOutput(); err != nil {
				t.Fatalf("git add: %v\n%s", err, out)
			}
			msg := filepath.Join(root, ".git", "COMMIT_EDITMSG")
			if err := os.WriteFile(msg, []byte("feat: x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			c := testApp.newHookCommand()
			c.SetArgs([]string{"prepare-commit-msg", msg, "message"})
			if err := c.Execute(); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(msg)
			if stamped := strings.Contains(string(got), "Agent-Session-Id: "+sid); stamped != gitHooks {
				t.Errorf("stamped %v, want %v:\n%s", stamped, gitHooks, got)
			}
			sp, err := spool.Open(filepath.Join(testApp.stateDir, spool.Dir))
			if err != nil {
				t.Fatal(err)
			}
			events := hookruntest.Spooled(t, sp)
			if len(events) == 0 {
				t.Fatal("nothing spooled")
			}
			for _, ev := range events {
				if ev.Enrollment != hookruntest.Enrollment(t, st, "acme-platform") || ev.Attrs[hookrun.AttrProjectID] != nil {
					t.Errorf("%s: enrollment %q, attrs %v", ev.Name, ev.Enrollment, ev.Attrs)
				}
			}
		})
	}
}
