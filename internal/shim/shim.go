// Package shim is what is left of per-repository agent routing, which terma no longer
// does: every agent's telemetry is configured once, machine-wide, by `terma setup`, and
// terma's relay sends each session to its repository's project (docs/RELAY.md).
//
// An earlier terma put a PATH shim in front of Claude Code and Codex that called `terma
// shim prepare` before starting the agent, wrote a routing record per project and a
// Claude settings document per project, and put the shim directory on PATH in the shell's
// startup file. This package keeps three things for machines that still have that:
//
//   - the launcher's protocol, answered with "no arguments" (Prepare), so a shim left
//     behind starts the real agent unchanged until it is removed;
//   - RemoveAll, which takes every piece of it away (`terma setup`, `terma install`,
//     `terma update --refresh` and `terma shim uninstall` call it);
//   - the routing record's shape, read by code that must tolerate one an older terma
//     wrote (LoadRecord), and migrated by migration 1.
package shim

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

// The agents an earlier terma routed, by binary name.
const (
	AgentClaude = "claude"
	AgentCodex  = "codex"
)

// CodexRoutedEnv marked a Codex CLI launch the shim had routed with runtime overrides.
// No launch sets it any more; hooks still read it so an old launcher's session is not
// mistaken for anything else.
const CodexRoutedEnv = "TERMA_CODEX_ROUTED"

// Record is the per-project routing record an earlier `terma install` wrote under
// config.Dir(). Nothing writes one now; RemoveAll deletes them.
type Record struct {
	ProjectID          string            `json:"project_id"`
	Endpoint           string            `json:"endpoint"`
	Signals            []string          `json:"signals"`
	CLI                bool              `json:"cli"`
	Desktop            bool              `json:"desktop"`
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

// RoutingDir held one <projectID>.json per routed project.
func RoutingDir() (string, error) { return dir("routing") }

// ShimBinDir is the directory the PATH-shim scripts were written to.
func ShimBinDir() (string, error) { return dir("shim", "bin") }

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

// SaveRecord writes a routing record in the shape an earlier terma did. Nothing in terma
// calls it; it is how a test sets up the state such a terma left behind.
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

// LoadRecord reads a project's routing record, if an earlier terma left one. ok is false
// when none exists.
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

// Exec runs the real agent binary with args, unchanged. On Unix it replaces the current
// process (execve), so signals, stdio and the exit status pass straight through;
// elsewhere it runs a child and exits with its status.
func Exec(agent string, args []string) error {
	binary, err := RealBinary(agent)
	if err != nil {
		return err
	}
	return execReal(binary, args, os.Environ())
}

// RealBinary finds the agent's binary on PATH, skipping the shim directory so a shim an
// earlier terma left behind never resolves to itself.
func RealBinary(agent string) (string, error) {
	shimDir, _ := ShimBinDir()
	// Walk PATH ourselves rather than stripping the shim directory out of the process
	// environment. exec.LookPath on a path with a separator resolves that exact directory
	// and keeps its per-OS executable rules (the Windows PATHEXT search included).
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || (shimDir != "" && sameDir(dir, shimDir)) {
			continue
		}
		candidate := filepath.Join(dir, agent)
		// A "." entry joins to a bare name, which exec.LookPath would resolve against the
		// whole PATH again — re-including the shim directory. Force a separator so the
		// lookup stays confined to this one directory.
		if !strings.ContainsRune(candidate, filepath.Separator) {
			candidate = "." + string(filepath.Separator) + candidate
		}
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("%s is not installed (not on PATH)", agent)
}

// RemoveAll tears down every trace of per-repo routing on this machine: the PATH-shim
// scripts and the block that put them on PATH, all projects' routing records, all
// per-project Claude settings. Keystore keys are left — they belong to the spool and to
// the machine's telemetry configuration too.
func RemoveAll() error {
	if err := RemoveShims(); err != nil {
		return err
	}
	if rc, ok := ShellRC(); ok {
		if _, err := rc.Remove(); err != nil {
			return err
		}
	}
	for _, sub := range []string{"routing", "claude", "shim"} {
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
