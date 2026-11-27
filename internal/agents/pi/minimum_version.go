package pi

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// minimumVersion is the first Pi terma's extension records in: 1.1.0 brought the run times of
// calls and tools, 1.5.0 the flag that tells a provider's billed cost from Pi's own estimate.
// The extension's MIN_PI is the same string, and stays inert below it.
const minimumVersion = "1.5.0"

// detect is the pi on PATH and the version its --version prints.
func detect(ctx context.Context) harness.Detection {
	return harness.DetectBinary(ctx, "pi", harness.SemverRE)
}

// tooOld reports the version of the pi on PATH when it predates minimumVersion, as an error
// setup and doctor both show; the retired @mariozechner Pi (0.x) is one. A pi setup cannot
// find or read the version of passes.
func tooOld(ctx context.Context) (string, bool) {
	return tooOldVersion(detect(ctx).Version)
}

// tooOldVersion is tooOld for a version pi --version printed.
func tooOldVersion(version string) (string, bool) {
	found, ok := parseVersion(version)
	if !ok {
		return "", false
	}
	minimum, _ := parseVersion(minimumVersion)
	if slices.Compare(found, minimum) >= 0 {
		return "", false
	}
	return "Pi " + minimumVersion + " or later is required (found " + version + "): terma's extension records nothing on an earlier Pi. Upgrade Pi (pi update, or npm install -g @earendil-works/pi-coding-agent)", true
}

// parseVersion is a version's major, minor and patch, as pi --version prints it: a missing
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
