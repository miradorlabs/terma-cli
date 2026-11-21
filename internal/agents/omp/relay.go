package omp

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// The extension terma writes into omp's agent directory, named apart from Pi's, which a
// PI_CODING_AGENT_DIR both read would share; and the files an earlier terma wrote there: an
// extension that exported spans itself, and a hook that set the exporter.
var (
	extensionPath = filepath.Join("extensions", "terma-omp.ts")
	legacyPaths   = []string{filepath.Join("extensions", "terma-relay.ts"), filepath.Join("hooks", "pre", "terma.ts")}
)

// ConfigureRelay writes the extension that reports sessions and file edits (it exports
// nothing itself), then points omp's native exporter at the local relay through its .env.
// The extension goes first: without its claims the relay holds omp's spans and drops them.
func (a Agent) ConfigureRelay(_ context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	dir, err := ompAgentDir()
	if err != nil {
		return agents.RelayResult{}, err
	}
	ext, err := writeExtension(filepath.Join(dir, extensionPath), cfg.HookCommand, cfg.Endpoint)
	if err != nil {
		return agents.RelayResult{}, err
	}
	result, err := relayexport.Native(a.Harness(), cfg)
	if err != nil {
		return result, err
	}
	if path, err := a.Harness().ConfigPath(); err == nil {
		result.Paths = append(result.Paths, path)
	}
	result.Paths = append(result.Paths, ext)
	removed, err := removeOwn(dir, legacyPaths)
	result.Paths = append(result.Paths, removed...)
	return result, err
}

// UnconfigureRelay restores omp's .env and removes terma's extension.
func (a Agent) UnconfigureRelay(context.Context, string) (agents.RelayResult, error) {
	dir, err := ompAgentDir()
	if err != nil {
		return agents.RelayResult{}, err
	}
	result, err := relayexport.Unnative(a.Harness())
	if err != nil {
		return result, err
	}
	removed, err := removeOwn(dir, append([]string{extensionPath}, legacyPaths...))
	result.Paths = append(result.Paths, removed...)
	return result, err
}

// RelayPointed reports whether omp's .env points its exporter at the relay at addr, with no
// shell export that omp reads ahead of the .env taking the export elsewhere or switching it
// off, and terma's extension in place to claim the sessions it exports.
func (a Agent) RelayPointed(addr string) (bool, bool) {
	if !relayexport.NativePointed(a.Harness(), addr) {
		return false, true
	}
	if _, _, problem := a.RelayProblem(addr); problem {
		return false, true
	}
	st, err := a.Harness().Status()
	return err == nil && !slices.ContainsFunc(st.Conflicts, func(c harness.Conflict) bool { return !c.Advisory }), true
}

// RelayProblem reports terma's extension missing from omp's agent directory, or not the
// hooks-only file this terma writes: without its claims the relay holds omp's spans and drops them.
func (Agent) RelayProblem(string) (detail, fix string, problem bool) {
	dir, err := ompAgentDir()
	if err != nil {
		return "", "", false
	}
	path := filepath.Join(dir, extensionPath)
	if extensionCurrent(path) {
		return "", "", false
	}
	return "OMP's terma extension " + path + " is missing or not this terma's, so no hook claims OMP's sessions and the relay drops their telemetry", "terma setup", true
}

// removeOwn deletes the files under dir among rels that terma wrote, each opening with its
// "// Terma " header; a file of the developer's own by the same name stays.
func removeOwn(dir string, rels []string) ([]string, error) {
	var removed []string
	for _, rel := range rels {
		path := filepath.Join(dir, rel)
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return removed, err
		}
		if !bytes.HasPrefix(data, []byte("// Terma ")) {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return removed, err
		}
		removed = append(removed, path)
	}
	return removed, nil
}

var (
	_ agents.RelayExporter = Agent{}
	_ agents.RelayChecker  = Agent{}
)
