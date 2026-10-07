package doctor

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// The recommendation is for a team whose policy is in force and collects something but
// installs no commit hook; with no policy, an expired one, or nothing collected, turning
// hooks on is not what the developer needs to hear.
func TestGitHooksOffOnlyForAPolicyInForceThatCollects(t *testing.T) {
	now := time.Now()
	off := func(edit func(*config.Policy)) config.Policy {
		p := stamping()
		p.GitHooks = false
		if edit != nil {
			edit(&p)
		}
		return p
	}
	for name, tc := range map[string]struct {
		policy config.Policy
		want   bool
	}{
		"off by policy":       {off(nil), true},
		"off in global mode":  {off(func(p *config.Policy) { p.Mode, p.Repositories = config.ModeGlobal, nil }), true},
		"on":                  {stamping(), false},
		"no policy at all":    {config.NoPolicy("org", "https://auth.example"), false},
		"expired":             {off(func(p *config.Policy) { p.FetchedAt = now.Add(-config.MaxPolicyAge - time.Hour) }), false},
		"lists no repository": {off(func(p *config.Policy) { p.Repositories = nil }), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := GitHooksOff(tc.policy, now); got != tc.want {
				t.Fatalf("GitHooksOff = %v, want %v", got, tc.want)
			}
		})
	}
}

// Doctor's commit-hooks line under a policy with hooks off is a skip, never a warning, and
// carries the recommendation as its → line; with hooks on it does not.
func TestDoctorRecommendsCommitHooksWhenThePolicyHasThemOff(t *testing.T) {
	root := unboundRepo(t)
	gitDir := filepath.Join(root, ".git")
	off := stamping()
	off.GitHooks = false
	c := (&run{env: Env{GitDir: gitDir, Root: root}, pol: off}).commitHooks()
	if c.Status != Skip || c.Fix != GitHooksOffStep {
		t.Fatalf("hooks off: %+v, want a skip with the recommendation", c)
	}
	var out strings.Builder
	RenderCheck(&out, Check{Name: "commit hooks in effect", Status: c.Status, Detail: c.Detail, Fix: c.Fix}, NameWidth)
	if !strings.Contains(out.String(), "→ For commit-level accuracy") {
		t.Fatalf("the recommendation is not printed:\n%s", out.String())
	}
	var summary strings.Builder
	RenderSummary(&summary, Report{Checks: []Check{{Key: KeyHooks, Status: c.Status, Detail: c.Detail, Fix: c.Fix}}})
	if strings.Contains(summary.String(), "needs attention") {
		t.Fatalf("an informational recommendation reads as setup needing attention:\n%s", summary.String())
	}

	if c := (&run{env: Env{GitDir: gitDir, Root: root}, pol: stamping()}).commitHooks(); c.Fix == GitHooksOffStep {
		t.Fatalf("hooks on still recommends turning them on: %+v", c)
	}
	if c := (&run{env: Env{GitDir: gitDir, Root: root}, pol: config.NoPolicy("org", "https://auth.example")}).commitHooks(); c.Fix != "" {
		t.Fatalf("no policy recommends commit hooks: %+v", c)
	}
}
