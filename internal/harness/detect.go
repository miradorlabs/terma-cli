package harness

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// semverRE pulls the version out of what a `--version` prints around it: Claude Code's
// "2.1.159 (Claude Code)", whose parenthetical changes; Codex's "codex-cli 0.152.0".
var semverRE = regexp.MustCompile(`\d+\.\d+\.\d+[^\s]*`)

// detectTimeout bounds the version probe: a hung binary should not hang the command.
const detectTimeout = 5 * time.Second

// detectBinary looks name up on PATH and asks it for its version. A missing binary is
// not-found rather than an error: connecting an uninstalled agent is allowed, since its
// config is read whenever it is eventually started. One that will not say its version
// is still installed, which is what matters.
func detectBinary(ctx context.Context, name string, version *regexp.Regexp) Detection {
	path, err := exec.LookPath(name)
	if err != nil {
		return Detection{}
	}
	ctx, cancel := context.WithTimeout(ctx, detectTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return Detection{Found: true, Path: path}
	}
	return Detection{Found: true, Path: path, Version: version.FindString(strings.TrimSpace(string(out)))}
}
