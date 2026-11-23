package antigravity

import (
	"context"
	"slices"
	"strconv"
	"strings"
)

// minimumVersion is the first agy whose hooks terma can rely on, by agy's changelog: Stop
// hooks run at all from 1.1.10 (before, they sat behind the built-in termination checks,
// so no turn ended in a flush), PostToolUse fires for tool steps alone from 1.1.9, and
// 1.2.4 stopped dropping hooks.json when the customization budget truncated. The
// adapter's oldest recorded payloads are 1.2.4's. agy 1.5.4 added the payload's cwd; an
// earlier one is placed by its first workspace path.
const minimumVersion = "1.2.4"

// tooOld reports the version of the agy on PATH when it predates minimumVersion, as an error
// setup and doctor both show. An agy whose version cannot be read passes: doctor names it
// as unknown.
func tooOld(ctx context.Context) (string, bool) {
	version := detect(ctx).Version
	found, ok := parseVersion(version)
	if !ok {
		return "", false
	}
	minimum, _ := parseVersion(minimumVersion)
	if slices.Compare(found, minimum) >= 0 {
		return "", false
	}
	return "Antigravity " + minimumVersion + " or later is required (found " + version + "): an earlier agy does not run terma's Stop hook", true
}

// parseVersion is a version's major, minor and patch: a missing part is 0, and a
// pre-release or build suffix is ignored. ok is false when the major is no number.
func parseVersion(v string) ([]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+ "); i >= 0 {
		v = v[:i]
	}
	parts := make([]int, 3)
	for i, s := range strings.SplitN(v, ".", 3) {
		n, err := strconv.Atoi(s)
		if err != nil {
			if i == 0 {
				return nil, false
			}
			break
		}
		parts[i] = n
	}
	return parts, true
}
