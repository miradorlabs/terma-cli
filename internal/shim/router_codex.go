package shim

import (
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/keystore"
)

// AgentCodex is Codex's binary name and its key in the keystore and the record.
const AgentCodex = "codex"

// CodexRoutedEnv marks a Codex process launched with Terma's runtime export
// overrides. Repository hooks use it to distinguish CLI consent from the
// desktop app's machine-wide exporter.
const CodexRoutedEnv = "TERMA_CODEX_ROUTED"

// codexRouter routes Codex through runtime `-c` overrides, preserving the developer's
// CODEX_HOME. Codex accepts an explicit working-directory flag, so its binding is looked
// up from that directory, not the shell's cwd.
type codexRouter struct{}

func (codexRouter) name() string { return AgentCodex }

func (codexRouter) workingDir(cwd string, userArgs []string) string {
	return codexWorkingDir(cwd, userArgs)
}

func (codexRouter) routeArgs(rec Record, _ []string) []string {
	key := keystore.GetFor(AgentCodex, rec.ProjectID)
	if key == "" {
		return nil
	}
	return (harness.Codex{}).RuntimeArgs(exporterFor(rec, key))
}

// codexWorkingDir resolves Codex's explicit working-directory override before looking up
// a binding. The arguments are left unchanged for Codex to validate and execute. A
// relative override is relative to the invoking shell, not a previous -C option.
func codexWorkingDir(cwd string, args []string) string {
	dir := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break // everything following this is positional content
		}
		switch {
		case a == "-C" || a == "--cd":
			if i+1 < len(args) {
				i++
				dir = args[i]
			}
		case strings.HasPrefix(a, "--cd="):
			dir = strings.TrimPrefix(a, "--cd=")
		case strings.HasPrefix(a, "-C"):
			dir = strings.TrimPrefix(strings.TrimPrefix(a, "-C"), "=")
		default:
			// Do not interpret a value belonging to another option as a cwd flag.
			switch a {
			case "-c", "--config", "-m", "--model", "-p", "--profile",
				"-s", "--sandbox", "-a", "--ask-for-approval", "-i", "--image",
				"--local-provider", "--add-dir", "--enable", "--disable",
				"-o", "--output-last-message", "--output-schema", "--color",
				"--thread-source", "--base", "--commit", "--title":
				i++
			}
		}
	}
	if dir == "" {
		return cwd
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(cwd, dir)
	}
	return filepath.Clean(dir)
}
