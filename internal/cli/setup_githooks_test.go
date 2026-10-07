package cli

import (
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
)

// A team whose policy installs no commit hook is told how to get commit-level accuracy, as
// a next step, never a step marked !. With hooks on it is not told.
func TestSetupRecommendsCommitHooksWhenThePolicyHasThemOff(t *testing.T) {
	for _, tc := range []struct {
		gitHooks string
		want     bool
	}{{"false", true}, {"true", false}} {
		t.Run("git_hooks "+tc.gitHooks, func(t *testing.T) {
			gateway := newFakeAuth(t)
			authSandbox(t, gateway)
			sandboxMachine(t)
			t.Setenv("CODEX_HOME", t.TempDir())
			t.Setenv("TERMA_POLICY_STUB", `{"mode":"repo","repositories":["github.com/acme/app"],"include_prompts":true,"include_tool_content":true,"git_hooks":`+tc.gitHooks+`,"default_project_id":"team"}`)
			if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(gateway, orgA())); err != nil {
				t.Fatal(err)
			}
			out, err := runTerma(t, "setup", "--harness", "codex")
			if err != nil {
				t.Fatalf("setup: %v\n%s", err, out)
			}
			if got := strings.Contains(out, doctor.GitHooksOffStep); got != tc.want {
				t.Errorf("recommendation shown = %v, want %v:\n%s", got, tc.want, out)
			}
			if i := strings.Index(out, doctor.GitHooksOffStep); i >= 0 && !strings.Contains(out[:i], "Next steps:") {
				t.Errorf("the recommendation is not a next step:\n%s", out)
			}
			if strings.Contains(out, "! Commits") {
				t.Errorf("the recommendation is marked as a step that needs the developer:\n%s", out)
			}
		})
	}
}
