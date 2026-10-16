package opencode

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// minimumOpenCode is the oldest OpenCode the plugin loads in. Up to 1.2.10 OpenCode called
// every export as a plugin function, so the default export OpenCode 2 reads made OpenCode
// fail to start; 1.2.11 skips it and runs the named one (measured with each release's own
// binary).
const minimumOpenCode = "1.2.11"

// opencodeSetupExport is the OpenCode 2 plugin: the default export's setup. A plugin
// terma wrote before it exports only OpenCode 1's plugin function, which OpenCode 2
// declines to load.
var opencodeSetupExport = regexp.MustCompile(`(?m)^export default \{[^}\n]*\bsetup\b`)

// versionTimeout bounds how long doctor, setup and refresh wait for `opencode --version`.
const versionTimeout = 3 * time.Second

// installedVersion is the version `opencode --version` reports, "" when there is no
// OpenCode or it says none. It runs the local binary only, and is a variable so tests
// can name a version.
var installedVersion = func() string {
	ctx, cancel := context.WithTimeout(context.Background(), versionTimeout)
	defer cancel()
	return detect(ctx).Version
}

// termaExecutable is the terma a repaired hook command runs: the path this one was
// started as, as setup names it.
var termaExecutable = procinfo.AbsExecutable

// detections remember what each opencode binary said, by its path, size and modification
// time, so one doctor or setup asks each build once.
var detections sync.Map

func detect(ctx context.Context) harness.Detection {
	key := ""
	if path, err := exec.LookPath("opencode"); err == nil {
		if info, err := os.Stat(path); err == nil {
			key = fmt.Sprintf("%s\x00%d\x00%d", path, info.Size(), info.ModTime().UnixNano())
			if d, ok := detections.Load(key); ok {
				return d.(harness.Detection)
			}
		}
	}
	d := harness.DetectBinary(ctx, "opencode", harness.SemverRE)
	if key != "" && d.Version != "" {
		detections.Store(key, d)
	}
	return d
}

// major is a version's major number, 0 when it has none.
func major(version string) int {
	n, _ := strconv.Atoi(strings.SplitN(version, ".", 2)[0])
	return n
}

// supported reports whether the plugin loads in OpenCode version; an unknown version is
// given the benefit of the doubt.
func supported(version string) bool {
	if version == "" {
		return true
	}
	have, want := strings.SplitN(version, ".", 3), strings.Split(minimumOpenCode, ".")
	for i := range want {
		if i >= len(have) {
			return false
		}
		h, _ := strconv.Atoi(strings.TrimFunc(have[i], func(r rune) bool { return r < '0' || r > '9' }))
		w, _ := strconv.Atoi(want[i])
		if h != w {
			return h > w
		}
	}
	return true
}

func tooOld(version string) string {
	return fmt.Sprintf("OpenCode %s is older than %s, the first that loads terma's plugin.", version, minimumOpenCode)
}

// hookGone reports why a hook command's terma will not run: named bare, which OpenCode 2's
// background server may not find on its PATH, or by a path where no terma is now. "" is a
// command that runs.
func hookGone(command []string) (bare bool, missing string) {
	if len(command) == 0 || command[0] == "" {
		return false, ""
	}
	path := command[0]
	if !filepath.IsAbs(path) {
		return true, ""
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return false, path
	}
	return false, ""
}

// repairHookCommand points a command that names terma bare, or by a path that is gone, at
// this terma, as setup would.
func repairHookCommand(command []string) []string {
	bare, missing := hookGone(command)
	if !bare && missing == "" {
		return command
	}
	exe, err := termaExecutable()
	if err != nil {
		return command
	}
	return append([]string{exe}, command[1:]...)
}

// RelayProblem reports a plugin that will not report: the installed OpenCode is older
// than it supports, its terma is gone, or the installed OpenCode 2 does not load it or
// may not find terma.
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
	if !ok {
		return "", "", false
	}
	version := installedVersion()
	plugin := output.TildePath(path)
	bare, missing := hookGone(cfg.HookCommand)
	switch {
	case !supported(version):
		return tooOld(version) + " Run `opencode upgrade`, then `terma setup` again.", "opencode upgrade, then terma setup", true
	case missing != "":
		return fmt.Sprintf("terma's OpenCode plugin (%s) runs terma at %s, which is not there now, so its commits go unattributed; run `terma setup` again to name this terma", plugin, missing),
			"terma setup", true
	case major(version) < 2:
		return "", "", false
	case !opencodeSetupExport.Match(data):
		return fmt.Sprintf("terma's OpenCode plugin (%s) was written for OpenCode 1, and the installed OpenCode 2 does not load it, so its sessions reach neither the relay nor commit attribution; run `terma setup` again to rewrite it", plugin),
			"terma setup", true
	case bare:
		return fmt.Sprintf("terma's OpenCode plugin (%s) runs %q by name, which OpenCode 2's background server may not find, so its commits may go unattributed; run `terma setup` again to name terma by its path", plugin, cfg.HookCommand[0]),
			"terma setup", true
	}
	return "", "", false
}

var _ agents.RelayChecker = Agent{}
