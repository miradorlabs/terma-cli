package grok

import (
	"context"
	"slices"
	"strconv"
	"strings"
)

// minimumVersion is the first grok terma was verified against: its hook payloads, its export
// and the [telemetry] keys terma writes are 1.0.92's.
const minimumVersion = "1.0.92"

// tooOld reports the version of the grok on PATH when it predates minimumVersion, as an error
// setup and doctor both show. A grok setup cannot find or read the version of passes.
func tooOld(ctx context.Context) (string, bool) {
	return tooOldVersion(exporter{}.Detect(ctx).Version)
}

// tooOldVersion is tooOld for a version grok --version printed.
func tooOldVersion(version string) (string, bool) {
	found, ok := parseVersion(version)
	if !ok {
		return "", false
	}
	minimum, _ := parseVersion(minimumVersion)
	if slices.Compare(found, minimum) >= 0 {
		return "", false
	}
	return "Grok Build " + minimumVersion + " or later is required (found " + version + "): terma reads the hooks and the export of " + minimumVersion + " and later", true
}

// parseVersion is a version's major, minor and patch, as grok --version prints it: a missing
// part is 0, and a pre-release or build suffix is ignored. ok is false when the major is no
// number.
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
