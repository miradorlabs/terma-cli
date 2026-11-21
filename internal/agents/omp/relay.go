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
	"github.com/miradorlabs/terma-cli/internal/agents/internal/pifamily"
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

// ConfigureRelay points omp's native exporter at the local relay through its .env, and
// writes the extension that reports sessions and file edits; it exports nothing itself.
func (a Agent) ConfigureRelay(_ context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	dir, err := ompAgentDir()
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
	ext, err := pifamily.Write(filepath.Join(dir, extensionPath), pifamily.ForHooks(name, cfg))
	if err != nil {
		return result, err
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
// shell export that omp reads ahead of the .env taking the export elsewhere or switching it off.
func (a Agent) RelayPointed(addr string) (bool, bool) {
	if !relayexport.NativePointed(a.Harness(), addr) {
		return false, true
	}
	st, err := a.Harness().Status()
	return err == nil && !slices.ContainsFunc(st.Conflicts, func(c harness.Conflict) bool { return !c.Advisory }), true
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

var _ agents.RelayExporter = Agent{}
