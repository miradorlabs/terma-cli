package harness

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// The resource attributes Terma stamps on everything a harness emits. These are the
// join keys: without service.name a trace cannot be filtered to one harness, and
// without enduser.id a team's traces cannot be told apart.
const (
	// AttrServiceName is what a trace filter such as `service.name="claude-code"`
	// matches on. Set explicitly rather than relying on the harness's default, so the
	// documented filter keeps working if that default ever changes.
	AttrServiceName = "service.name"

	// AttrEnduserID attributes a trace to a person. Resolved from git's configured
	// email, which is the identity already attached to the work being traced.
	AttrEnduserID = "enduser.id"

	// AttrProjectID records which Terma project the harness was connected against, so
	// `status` can report the project the harness actually reports to rather than
	// whichever one happens to be selected now. Codex and OpenCode carry it as a span
	// attribute; Claude Code's settings carry no resource attributes, so for it the id
	// lives in the connect journal instead.
	AttrProjectID = "mirador.project.id"
)

// ServiceName is the service.name a harness reports under, which is what the filter
// printed after a connect matches on.
//
// Claude Code's is its own default, "claude-code", which Terma does not override (it
// writes no OTEL_RESOURCE_ATTRIBUTES; see claudeRetiredKeys). Codex's is not
// configurable: it stamps its own originator on every resource, and for the `codex` CLI
// that is "codex_cli_rs". The Codex desktop app and IDE extensions report under their
// own originators, which this harness does not configure. OpenCode's is stamped by
// Terma's plugin.
func ServiceName(h Harness) string {
	switch h.Name() {
	case "claude":
		return "claude-code"
	case "codex":
		return codexServiceName
	case "opencode":
		return opencodeServiceName
	default:
		return h.Name()
	}
}

// GitEmail returns the email from git's *global* configuration, or "" when git is
// absent or has none set.
//
// Global specifically, not the repository's effective value. This identity is written
// into the harness's global configuration, where it labels every future session from
// this machine — so reading a repository-local `user.email` would take the work address
// configured in one checkout and stamp it on personal sessions in every other directory,
// permanently and invisibly. A machine-wide file deserves a machine-wide answer;
// `--identity` exists for anyone who wants a different one.
//
// The value is resolved here and written as a literal. It cannot be left as a shell
// substitution: harness config files hold literal strings, so a
// `$(git config user.email)` written into one is exported verbatim as that text.
func GitEmail(ctx context.Context) string {
	path, err := exec.LookPath("git")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, path, "config", "--global", "--get", "user.email").Output()
	if err != nil {
		// Unset is the ordinary case (exit 1), not a failure worth reporting. There is
		// deliberately no fallback to the local value: being wrong about who a session
		// belongs to is worse than not saying.
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Hostname is the fallback identity when git has no email configured, and the default
// suffix for a minted key's name.
func Hostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "unknown-host"
	}
	// A hostname like "work-laptop.local" reads better trimmed to its first label.
	if short, _, found := strings.Cut(name, "."); found && short != "" {
		return short
	}
	return name
}
