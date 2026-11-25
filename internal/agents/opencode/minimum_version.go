package opencode

import (
	"context"
	"slices"
	"strconv"
	"strings"
)

// minimumVersions is the first build of each OpenCode major terma's plugin works with, by
// major. 1.16.0 is the first OpenCode 1 that calls the plugin's dispose hook, where the
// plugin sends what it still holds: an earlier one exits with the last batch unsent. A major
// not listed passes.
var minimumVersions = map[int]string{
	1: "1.16.0",
	2: "2.0.0",
}

// tooOld reports the version of the OpenCode on PATH when it predates the minimum for its
// major, as an error setup and doctor both show. An OpenCode setup cannot find or read the
// version of passes.
func tooOld(ctx context.Context) (string, bool) {
	return tooOldVersion(exporter{}.Detect(ctx).Version)
}

// tooOldVersion is tooOld for a version opencode --version printed.
func tooOldVersion(version string) (string, bool) {
	found, ok := parseVersion(version)
	if !ok {
		return "", false
	}
	minimumVersion, listed := minimumVersions[found[0]]
	if !listed {
		return "", false
	}
	minimum, _ := parseVersion(minimumVersion)
	if slices.Compare(found, minimum) >= 0 {
		return "", false
	}
	return displayName + " " + minimumVersion + " or later is required (found " + version + "): terma's plugin exports nothing from an earlier " + strconv.Itoa(found[0]) + ".x", true
}

// parseVersion is a version's major, minor and patch, as opencode --version prints it: a
// missing part is 0, and a pre-release or build suffix is ignored. ok is false when the
// major is no number.
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
