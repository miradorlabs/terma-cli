// Package shim delivers per-repository agent routing: when a coding agent runs inside
// a repository bound to a Terma project, it exports to that project rather than to one
// machine-wide destination.
//
// The secrets never move into the repository. Keys and per-project configuration live
// in the home directory (config.Dir()), namespaced by project id; the committed
// .terma/settings.json only names the project. Two delivery mechanisms sit on top of
// the same resolver:
//
//   - a PATH shim: a directory of tiny scripts (one per agent) placed ahead of the real
//     binaries, each of which calls `terma shim prepare <agent>` before execing the agent;
//   - an opt-in wrapper: shell functions the developer pastes into their rc, which call
//     the same installed shell launcher.
//
// `routeFor` resolves launch arguments when the current directory belongs to a bound
// repository whose project has a routing record:
//
//   - Claude Code is started with `--settings <per-project document>`, which names the
//     export and a headers helper holding the key. The command line is used because it
//     outranks ~/.claude/settings.json; the process environment does not, so a developer
//     connected machine-wide would otherwise never be routed (see
//     harness.Claude.WriteRouteSettings);
//   - Codex receives telemetry through runtime -c overrides, preserving CODEX_HOME
//     and all existing authentication, configuration, trust, and notify state.
//
// Outside a bound repository, or for an agent with no record, Exec is a transparent
// pass-through to the real binary.
package shim

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

// Per-repo routing is dispatched through the router registry in router.go; each agent's
// routing (Claude via a per-project `--settings` document, Codex via runtime -c overrides)
// lives in its own router_*.go. The agent-name constants live beside their routers.

// Record is a project's per-repo routing configuration, written under config.Dir() at
// install time and read by Exec. It holds no secret: the key stays in the keystore,
// keyed by harness and project, so revoking one project's key never exposes another's.
type Record struct {
	ProjectID          string            `json:"project_id"`
	Endpoint           string            `json:"endpoint"`
	Signals            []string          `json:"signals"`
	IncludePrompts     bool              `json:"include_prompts"`
	IncludeToolContent bool              `json:"include_tool_content"`
	ResourceAttributes map[string]string `json:"resource_attributes,omitempty"`
	// Harnesses names the agents routed to this project for this developer.
	Harnesses []string `json:"harnesses"`
}

func dir(parts ...string) (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{base}, parts...)...), nil
}

// RoutingDir holds one <projectID>.json per routed project.
func RoutingDir() (string, error) { return dir("routing") }

// ShimBinDir is the directory of PATH-shim scripts.
func ShimBinDir() (string, error) { return dir("shim", "bin") }

// WrapperEnv is exported by the opt-in shell wrapper (WrapperSnippet) so that a child
// process — `terma doctor`, `terma status` — can tell the wrapper is loaded. Shell
// functions themselves are invisible from a child.
const WrapperEnv = "TERMA_ROUTING"

// Active reports whether running `agent` from this environment goes through terma's
// routing: the name resolves to terma's shim (the shim directory is on PATH *ahead of*
// the real binary — being on PATH behind it routes nothing), or the shell wrapper is
// loaded. This, not the mere presence of the shim directory, is what makes per-repo
// routing live.
func Active(agent string) bool {
	if os.Getenv(WrapperEnv) == "wrapper" {
		return true
	}
	binDir, err := ShimBinDir()
	if err != nil {
		return false
	}
	found, err := exec.LookPath(agent)
	if err != nil {
		return false
	}
	return sameDir(filepath.Dir(found), binDir)
}

func recordPath(projectID string) (string, error) {
	if !termaproject.ValidID(projectID) {
		return "", fmt.Errorf("unsafe project id %q", projectID)
	}
	d, err := RoutingDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, projectID+".json"), nil
}

// SaveRecord writes (or replaces) a project's routing record.
func SaveRecord(rec Record) error {
	p, err := recordPath(rec.ProjectID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(p, append(data, '\n'), 0o600)
}

// LoadRecord reads a project's routing record. ok is false when none exists.
func LoadRecord(projectID string) (rec Record, ok bool, err error) {
	p, err := recordPath(projectID)
	if err != nil {
		return Record{}, false, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, false, err
	}
	return rec, true, nil
}

// exporterFor builds the exporter a router hands its harness for a record and key. The
// credential is inline (HelperPath empty); each harness renders it its own way — Claude
// into OTLP header env, Codex into -c overrides.
func exporterFor(rec Record, key string) harness.Exporter {
	signals := make([]harness.Signal, 0, len(rec.Signals))
	for _, s := range rec.Signals {
		signals = append(signals, harness.Signal(s))
	}
	return harness.Exporter{
		Endpoint:           rec.Endpoint,
		APIKey:             key,
		Signals:            signals,
		ResourceAttributes: rec.ResourceAttributes,
		ProjectID:          rec.ProjectID,
		IncludePrompts:     rec.IncludePrompts,
		IncludeToolContent: rec.IncludeToolContent,
	}
}

// route is how an agent is started so that it reports to the repository's project:
// extra environment, and arguments placed ahead of the agent's own. The zero route is a
// transparent pass-through.
type route struct {
	env  map[string]string
	args []string
}

// Exec runs the real agent binary, routed to the repository's project when that applies.
// On Unix it replaces the current process (execve), so signals, stdio and the exit
// status pass straight through; elsewhere it runs a child and exits with its status.
func Exec(agent string, args []string) error {
	binary, err := RealBinary(agent)
	if err != nil {
		return err
	}
	env := os.Environ()
	cwd, _ := os.Getwd()
	r := routeFor(agent, cwd, args)
	if len(r.env) > 0 {
		env = mergeEnv(env, r.env)
	}
	if len(r.args) > 0 {
		args = append(slices.Clone(r.args), args...)
	}
	return execReal(binary, args, env)
}

// RealBinary finds the agent's binary on PATH, skipping the shim directory so a PATH
// shim never re-invokes itself.
func RealBinary(agent string) (string, error) {
	shimDir, _ := ShimBinDir()
	// Walk PATH ourselves rather than stripping the shim directory out of the process
	// environment: a global os.Setenv for a lookup is un-idiomatic and unsafe if any
	// caller ever runs off the single-threaded exec path. exec.LookPath on a path with a
	// separator resolves that exact directory and keeps its per-OS executable rules
	// (the Windows PATHEXT search included).
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || (shimDir != "" && sameDir(dir, shimDir)) {
			continue
		}
		candidate := filepath.Join(dir, agent)
		// A "." entry joins to a bare name, which exec.LookPath would resolve against the
		// whole PATH again — re-including the shim directory and risking the shim resolving
		// itself. Force a separator so the lookup stays confined to this one directory.
		if !strings.ContainsRune(candidate, filepath.Separator) {
			candidate = "." + string(filepath.Separator) + candidate
		}
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("%s is not installed (not on PATH)", agent)
}

// InstallShims writes a PATH-shim script per agent into ShimBinDir. Each script
// prepares routing with a bounded Terma subprocess, then executes the dynamically
// resolved agent exactly once, with unchanged arguments if preparation fails.
// Returns the shim directory to add to PATH.
func InstallShims(agents []string) (string, error) {
	binDir, err := ShimBinDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", err
	}
	for _, agent := range agents {
		if !Routable(agent) {
			continue
		}
		script := shimScript(agent, binDir)
		if err := config.WriteFileAtomic(filepath.Join(binDir, agent), []byte(script), 0o755); err != nil {
			return "", err
		}
	}
	return binDir, nil
}

// RemoveAll tears down every trace of per-repo routing on this machine: the PATH-shim
// scripts and the block that put them on PATH, all projects' routing records, all
// per-project Claude settings. Keystore keys are left — they belong to the spool and to
// any global connects too.
func RemoveAll() error {
	if err := RemoveShims(); err != nil {
		return err
	}
	if rc, ok := ShellRC(); ok {
		if _, err := rc.Remove(); err != nil {
			return err
		}
	}
	for _, sub := range []string{"routing", "claude"} {
		d, err := dir(sub)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(d); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// RemoveShims deletes the PATH-shim scripts and the (now empty) directory.
func RemoveShims() error {
	binDir, err := ShimBinDir()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(binDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		_ = os.Remove(filepath.Join(binDir, e.Name()))
	}
	_ = os.Remove(binDir)
	return nil
}

// WrapperSnippet delegates to the installed launcher without changing PATH.
func WrapperSnippet(agents []string) string {
	var b strings.Builder
	b.WriteString("# terma per-repo routing — added by `terma install`.\n")
	b.WriteString("# Routes these agents to the repository's Terma project when run inside one.\n")
	b.WriteString("export " + WrapperEnv + "=wrapper\n")
	for _, agent := range agents {
		if !Routable(agent) {
			continue
		}
		binDir, _ := ShimBinDir()
		fmt.Fprintf(&b, "%s() { if [ -x %s ]; then command %s \"$@\"; else command %s \"$@\"; fi; }\n", agent, shellQuote(filepath.Join(binDir, agent)), shellQuote(filepath.Join(binDir, agent)), agent)
	}
	return b.String()
}

// mergeEnv overlays extra onto a base environment slice, replacing matching keys.
func mergeEnv(base []string, extra map[string]string) []string {
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		key := kv
		if before, _, ok := strings.Cut(kv, "="); ok {
			key = before
		}
		if _, ok := extra[key]; ok {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

// sameDir reports whether two paths name one directory. A PATH entry is whatever the
// developer typed into their rc — a trailing slash, a symlinked home — so comparing
// strings would miss the shim directory, and RealBinary would then hand back the shim
// itself and exec it in a loop.
func sameDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	return err == nil && os.SameFile(ai, bi)
}

// shellQuote wraps a path in single quotes for the /bin/sh shim, escaping any quote.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
