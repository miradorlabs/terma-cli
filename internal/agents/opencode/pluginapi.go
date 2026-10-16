package opencode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// opencodeSetupExport is the OpenCode 2 plugin: the default export's setup. A plugin
// terma wrote before it exports only OpenCode 1's plugin function, which OpenCode 2
// declines to load.
var opencodeSetupExport = regexp.MustCompile(`(?m)^export default \{[^}\n]*\bsetup\b`)

// installedMajor is the major version `opencode --version` reports, 0 when it says none.
// It runs the local binary only, and is a variable so tests can name a version.
var installedMajor = func() int {
	version := exporter{}.Detect(context.Background()).Version
	major, _, _ := strings.Cut(version, ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0
	}
	return n
}

// RelayProblem reports a plugin the installed OpenCode 2 cannot use: one written for
// OpenCode 1 alone, which it does not load, or one that runs terma by its bare name,
// which OpenCode 2's background server may not find on its PATH.
func (a Agent) RelayProblem() (string, string, bool) {
	path, err := exporter{dir: a.ConfigDir}.ConfigPath()
	if err != nil {
		return "", "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", false
	}
	cfg, ok := readPluginConfig(data)
	if !ok || installedMajor() < 2 {
		return "", "", false
	}
	plugin := output.TildePath(path)
	if !opencodeSetupExport.Match(data) {
		return fmt.Sprintf("terma's OpenCode plugin (%s) was written for OpenCode 1, and the installed OpenCode 2 does not load it, so its sessions reach neither the relay nor commit attribution; run `terma setup` again to rewrite it", plugin),
			"terma setup", true
	}
	if len(cfg.HookCommand) > 0 && !filepath.IsAbs(cfg.HookCommand[0]) {
		return fmt.Sprintf("terma's OpenCode plugin (%s) runs %q by name, which OpenCode 2's background server may not find, so its commits may go unattributed; run `terma setup` again to name terma by its path", plugin, cfg.HookCommand[0]),
			"terma setup", true
	}
	return "", "", false
}

var _ agents.RelayChecker = Agent{}
