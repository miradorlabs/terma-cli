package omp

import (
	"context"
	"slices"
	"strconv"
	"strings"
)

// minimumVersion is the first OMP that keeps its agent .env's OTEL_* settings from the tools
// it runs (OMP PR #17685, released in 18.18.2), and whose @oh-my-pi/pi-utils offers the
// helpers terma's extension asks for content with. An earlier one loads the .env into its
// environment, so every tool would inherit the relay's endpoints and the token in their path.
const minimumVersion = "18.18.2"

// tooOld reports the version of the omp on PATH when it predates minimumVersion, as an error
// setup and doctor both show. An omp setup cannot find or read the version of passes.
func tooOld(ctx context.Context) (string, bool) {
	version := exporter{}.Detect(ctx).Version
	found, ok := parseVersion(version)
	if !ok {
		return "", false
	}
	minimum, _ := parseVersion(minimumVersion)
	if slices.Compare(found, minimum) >= 0 {
		return "", false
	}
	return "OMP " + minimumVersion + " or later is required (found " + version + "): an earlier OMP hands the relay's token to every tool it runs", true
}

// parseVersion is a version's major, minor and patch, as omp --version prints it: a missing
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
