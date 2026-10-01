package harness

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// SemverRE pulls a semantic version out of whatever a `--version` prints around it.
var SemverRE = regexp.MustCompile(`\d+\.\d+\.\d+[^\s]*`)

const detectTimeout = 5 * time.Second

// DetectBinary looks name up on PATH and asks it for its version; one that will not say is still found.
func DetectBinary(ctx context.Context, name string, version *regexp.Regexp) Detection {
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
