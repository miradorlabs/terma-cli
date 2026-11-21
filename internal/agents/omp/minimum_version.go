package omp

import (
	"context"
	"strconv"
	"strings"
)

// minimumMajor is the first OMP that keeps its agent .env's OTEL_* settings from the tools it
// runs. An earlier one loads them into its environment, so every tool would inherit the
// relay's endpoints and the token in their path.
const minimumMajor = 19

// tooOld reports the version of the omp on PATH when it predates minimumMajor, as an error
// setup and doctor both show. An omp setup cannot find or read the version of passes.
func tooOld(ctx context.Context) (string, bool) {
	version := exporter{}.Detect(ctx).Version
	major, _, _ := strings.Cut(version, ".")
	n, err := strconv.Atoi(major)
	if err != nil || n >= minimumMajor {
		return "", false
	}
	return "OMP 19 or later is required (found " + version + "): an earlier OMP hands the relay's token to every tool it runs", true
}
