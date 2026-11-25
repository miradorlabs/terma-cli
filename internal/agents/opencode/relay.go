package opencode

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

const (
	setupFix = "terma setup"

	restartNote = `Restart OpenCode to load terma's plugin. A long-running "opencode serve", the OpenCode 2 background service or T3 Code's OpenCode server needs a restart too.`

	// refusedLine is how OpenCode 1 and 2 both log a plugin file they would not load.
	refusedLine = "failed to load plugin"
	// logTail is how much of OpenCode's newest log is read for that line.
	logTail = 256 << 10
	// maxReason is how much of OpenCode's reason doctor quotes.
	maxReason = 200
)

// ConfigureRelay refuses an OpenCode too old for terma's plugin before it writes anything.
// Otherwise it writes the plugin, pointed at the local relay with the relay's token and
// running this terma by its absolute path for its hooks.
func (a Agent) ConfigureRelay(ctx context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	if why, old := tooOld(ctx); old {
		return agents.RelayResult{}, errors.New(why)
	}
	h := exporter{dir: a.ConfigDir, hook: cfg.HookCommand}
	if err := h.Connect(harness.Exporter{Endpoint: cfg.Endpoint, APIKey: cfg.Token, Signals: harness.AllSignals}, true); err != nil {
		return agents.RelayResult{}, err
	}
	var result agents.RelayResult
	if path, err := h.ConfigPath(); err == nil {
		result.Paths = append(result.Paths, path)
	}
	result.Notes = append(result.Notes, restartNote)
	return result, nil
}

// UnconfigureRelay restores OpenCode's own exporter settings to what they were before setup.
func (a Agent) UnconfigureRelay(context.Context, string) (agents.RelayResult, error) {
	return relayexport.Unnative(exporter{dir: a.ConfigDir})
}

// RelayPointed reports whether terma's plugin sends OpenCode's sessions to the relay at addr:
// the plugin this build writes, pointed there, on an OpenCode new enough to run it, with a
// terma it can run, and loaded. A shell export of OpenCode's own is a warning, not a reason
// (RelayProblem). It is unknown where OpenCode is not on the PATH.
func (a Agent) RelayPointed(addr string) (bool, bool) {
	if !(exporter{}).Detect(context.Background()).Found {
		return false, false
	}
	if !relayexport.NativePointed(exporter{dir: a.ConfigDir}, addr) {
		return false, true
	}
	_, _, problem := a.relayProblem(addr)
	return !problem, true
}

// RelayProblem reports why OpenCode's sessions miss the relay at addr, or, where nothing
// does, the warning that a shell export sends OpenCode's own telemetry elsewhere as well.
// An OpenCode not on the PATH has none.
func (a Agent) RelayProblem(addr string) (detail, fix string, problem bool) {
	if !(exporter{}).Detect(context.Background()).Found {
		return "", "", false
	}
	if detail, fix, problem := a.relayProblem(addr); problem {
		return detail, fix, true
	}
	st, err := exporter{dir: a.ConfigDir}.Status()
	if err != nil {
		return "", "", false
	}
	if i := slices.IndexFunc(st.Conflicts, func(c harness.Conflict) bool { return c.Advisory }); i >= 0 {
		c := st.Conflicts[i]
		return displayName + ": " + c.Key + "=" + c.Value + " is " + c.Reason, "unset it", true
	}
	return "", "", false
}

// relayProblem is what keeps OpenCode's sessions from the relay at addr, the first that
// applies: an OpenCode too old for the plugin, the plugin missing, not this build's or
// pointed elsewhere, a terma it cannot run, or OpenCode refusing to load it.
func (a Agent) relayProblem(addr string) (detail, fix string, problem bool) {
	if why, old := tooOld(context.Background()); old {
		return why, "upgrade OpenCode, then run terma setup", true
	}
	h := exporter{dir: a.ConfigDir}
	path, err := h.ConfigPath()
	if err != nil {
		return "", "", false
	}
	cfg, current := pluginCurrent(path)
	if !current || !relayexport.NativePointed(h, addr) {
		return "OpenCode's terma plugin " + output.TildePath(path) + " is missing, not this terma's or not pointed at the local relay, so no hook claims OpenCode's sessions and nothing of them is exported", setupFix, true
	}
	if terma := cfg.HookCommand[0]; !runnable(terma) {
		return "OpenCode's terma plugin runs " + output.TildePath(terma) + ", which is not there or cannot be run, so the plugin cannot run terma and OpenCode sessions are not recorded", setupFix, true
	}
	if reason, refused := pluginRefused(path); refused {
		return "OpenCode refused to load terma's plugin " + output.TildePath(path) + ": " + reason, "terma setup, then restart OpenCode", true
	}
	return "", "", false
}

// pluginCurrent reads the plugin at path and reports whether it is what this build writes
// for the configuration it holds, which names the terma it runs.
func pluginCurrent(path string) (*opencodeConfig, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	cfg, ok := readPluginConfig(data)
	if !ok || len(cfg.HookCommand) == 0 {
		return nil, false
	}
	want, err := renderPlugin(*cfg)
	return cfg, err == nil && bytes.Equal(want, data)
}

// runnable reports whether the plugin can start command: an executable regular file, by
// its path or, for a bare name, on the PATH.
func runnable(command string) bool {
	if !strings.ContainsAny(command, `/\`) {
		found, err := exec.LookPath(command)
		if err != nil {
			return false
		}
		command = found
	}
	return executable(command)
}

// RelayTokenCurrent reports whether the plugin presents token, the relay's, as its bearer;
// known is false where there is no plugin, it is inert or it runs a headers helper.
func (a Agent) RelayTokenCurrent(token string) (tokenMatches, known bool) {
	path, err := exporter{dir: a.ConfigDir}.ConfigPath()
	if err != nil {
		return false, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, false
	}
	cfg, ok := readPluginConfig(data)
	if !ok || cfg.HeadersHelper != "" {
		return false, false
	}
	bearer, ok := cfg.Headers[opencodeAuthorizationHeader]
	if !ok {
		return false, false
	}
	return bearer == "Bearer "+token, true
}

// opencodeLogDir is where OpenCode writes its logs: $XDG_DATA_HOME/opencode/log, else
// ~/.local/share/opencode/log, on every platform.
func opencodeLogDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); dir != "" {
		return filepath.Join(dir, "opencode", "log"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "opencode", "log"), nil
}

// The time a log line was written, in each form OpenCode writes it, UTC: 1.19 and 2.x as
// timestamp=<RFC 3339 with milliseconds>, 1.16 as the line's second field, to the second.
var (
	logStamp       = regexp.MustCompile(`\btimestamp=(\S+)`)
	logStampSecond = regexp.MustCompile(`^\S+ (\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}) `)
)

// pluginRefused reports why OpenCode last refused to load terma's plugin at path, from its
// newest log: the last line that says so about this file since it was written. OpenCode
// logs a refusal as a warning nobody sees, and starts without the plugin.
func pluginRefused(path string) (string, bool) {
	written, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	dir, err := opencodeLogDir()
	if err != nil {
		return "", false
	}
	tail, ok := newestLogTail(dir)
	if !ok {
		return "", false
	}
	reason, refused := "", false
	for line := range strings.Lines(tail) {
		line = strings.TrimRight(line, "\r\n")
		if !strings.Contains(line, refusedLine) || !strings.Contains(line, opencodePluginFile) {
			continue
		}
		if at, precision, ok := logLineTime(line); ok && at.Before(written.ModTime().Truncate(precision)) {
			continue
		}
		reason, refused = refusalReason(line), true
	}
	return reason, refused
}

// newestLogTail is the end of the file in dir modified last.
func newestLogTail(dir string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	var newest string
	var at time.Time
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if newest == "" || info.ModTime().After(at) {
			newest, at = e.Name(), info.ModTime()
		}
	}
	if newest == "" {
		return "", false
	}
	f, err := os.Open(filepath.Join(dir, newest))
	if err != nil {
		return "", false
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > logTail {
		if _, err := f.Seek(-logTail, io.SeekEnd); err != nil {
			return "", false
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, logTail))
	if err != nil {
		return "", false
	}
	return string(data), true
}

// logLineTime is when line was written and the precision OpenCode wrote it to; ok is false
// where it names no time terma can read.
func logLineTime(line string) (time.Time, time.Duration, bool) {
	if m := logStamp.FindStringSubmatch(line); m != nil {
		if at, err := time.Parse(time.RFC3339Nano, m[1]); err == nil {
			return at, time.Millisecond, true
		}
	}
	if m := logStampSecond.FindStringSubmatch(line); m != nil {
		if at, err := time.ParseInLocation("2006-01-02T15:04:05", m[1], time.UTC); err == nil {
			return at, time.Second, true
		}
	}
	return time.Time{}, 0, false
}

// refusalReason is OpenCode's own reason in a refusal line, cut to maxReason characters:
// its cause= or error= field, else the line.
func refusalReason(line string) string {
	reason := line
	for _, field := range []string{`cause="`, `error="`} {
		if i := strings.Index(line, field); i >= 0 {
			reason = quoted(line[i+len(field):])
			break
		}
	}
	if reason == line {
		if i := strings.Index(line, "error="); i >= 0 {
			reason = strings.TrimSpace(strings.TrimSuffix(line[i+len("error="):], refusedLine))
		}
	}
	if r := []rune(reason); len(r) > maxReason {
		reason = string(r[:maxReason]) + "…"
	}
	return reason
}

// quoted is s up to its first quote no backslash escapes.
func quoted(s string) string {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return s[:i]
		}
	}
	return s
}

var (
	_ agents.RelayExporter     = Agent{}
	_ agents.RelayChecker      = Agent{}
	_ agents.RelayTokenChecker = Agent{}
)
