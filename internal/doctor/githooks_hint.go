package doctor

import (
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// GitHooksOffStep recommends commit hooks to a developer whose team policy installs none.
// It is information, not a fix: without the hooks, which work landed is inferred from the
// git commands agents run, and a commit made outside an agent is not seen as it lands.
const GitHooksOffStep = "Ask a team admin to turn on commit hooks in the Terma web app for commit-level accuracy; until then, which work landed is inferred from agent activity."

// GitHooksOff reports whether p is a policy in force that collects something but installs
// no commit hook: the one case where turning hooks on is the advice that helps.
func GitHooksOff(p config.Policy, now time.Time) bool {
	return p.Validated() && !p.Expired(now) && !p.GitHooks && !p.AdmitsNone()
}
