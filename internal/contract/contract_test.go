package contract

import (
	"bytes"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/builtin"
	"github.com/miradorlabs/terma-cli/internal/agents/codex"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

var update = flag.Bool("update", false, "rewrite the snapshots from the current build")

// terma is the absolute path a machine-wide hook entry names.
const terma = "/usr/local/bin/terma"

// TestCommittedHookFiles pins what `terma install` commits into an empty repository for
// each agent.
func TestCommittedHookFiles(t *testing.T) {
	for _, a := range builtin.Agents().All() {
		if a.HooksPath() == "" {
			continue // an agent whose hooks are user-scope commits nothing
		}
		t.Run(a.Name(), func(t *testing.T) {
			root := t.TempDir()
			plan, err := a.Plan(root, true)
			if err != nil {
				t.Fatal(err)
			}
			files := map[string][]byte{}
			for _, c := range plan.Changes {
				files[c.Path] = c.After
			}
			check(t, filepath.Join("committed", a.Name()), files, root)
		})
	}
}

// TestCodexTrustKeys pins each entry's key and hash, which Codex records trust under: a
// new hash is skipped until the developer trusts it again.
func TestCodexTrustKeys(t *testing.T) {
	root := t.TempDir()
	a, _ := builtin.Agents().Lookup("codex")
	plan, err := a.Plan(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := hookmgr.Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	entries, err := codex.TermaEntries(root)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.Key() + " " + e.Hash + "\n")
	}
	check(t, "trust", map[string][]byte{"codex-entries.txt": []byte(b.String())}, root)
}

// TestUserHookFiles pins the machine-wide hooks files setup writes.
func TestUserHookFiles(t *testing.T) {
	for _, a := range builtin.Agents().With[agents.UserHooks]() {
		t.Run(a.Name(), func(t *testing.T) {
			dir := t.TempDir()
			plan, err := a.PlanUserHooks(dir, hookmgr.UserHookCommand(terma), true)
			if err != nil {
				t.Fatal(err)
			}
			files := map[string][]byte{}
			for _, c := range plan.Changes {
				files[c.Path] = c.After
			}
			check(t, filepath.Join("user", a.Name()), files, dir)
		})
	}
}

// TestManagedConfiguration pins the files an organization deploys with `terma setup
// --managed-config`.
func TestManagedConfiguration(t *testing.T) {
	command := hookmgr.ManagedHookCommand("$HOME/.local/bin/terma")
	files := map[string][]byte{}
	for _, a := range builtin.Agents().With[agents.ManagedHooks]() {
		name, data, err := a.ManagedConfig(command)
		if err != nil {
			t.Fatal(err)
		}
		files[a.Name()+"/"+name] = data
	}
	check(t, "managed", files, "")
}

// TestRelayExporterFiles pins what pointing each agent at the local relay writes into
// its user-level configuration.
func TestRelayExporterFiles(t *testing.T) {
	for _, e := range builtin.Agents().With[agents.RelayExporter]() {
		t.Run(e.Name(), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("SHELL", "/bin/sh")
			t.Setenv("PATH", "") // Hermes's activation is then left to the developer.
			t.Setenv("TERMA_CONFIG_DIR", filepath.Join(home, ".terma-state"))
			for key, dir := range map[string]string{
				"CLAUDE_CONFIG_DIR": "claude", "CODEX_HOME": "codex", "XDG_CONFIG_HOME": "xdg",
				"PI_CODING_AGENT_DIR": "pi", "OMP_DIR": "omp", "HERMES_HOME": "hermes",
				"GEMINI_CLI_HOME": "gemini", "DSH_HOME": "dsh",
			} {
				t.Setenv(key, filepath.Join(home, dir))
			}
			state := filepath.Join(home, ".terma-state", "relay")
			if err := os.MkdirAll(state, 0o700); err != nil {
				t.Fatal(err)
			}
			_, err := e.ConfigureRelay(t.Context(), agents.RelayConfig{
				Endpoint: "http://127.0.0.1:43180", Token: "relay-token",
				HookCommand: []string{terma, "hook"}, StateDir: state,
			})
			if err != nil {
				t.Fatal(err)
			}
			files := map[string][]byte{}
			err = filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				rel, _ := filepath.Rel(home, path)
				if d.IsDir() {
					if rel == ".terma-state" {
						return filepath.SkipDir // terma's own state: journals and timestamps
					}
					return nil
				}
				body, err := os.ReadFile(path)
				files[filepath.ToSlash(rel)] = body
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			check(t, filepath.Join("relay", e.Name()), files, home)
		})
	}
}

// check compares files with the snapshot under testdata/dir, the temporary root
// replaced by a fixed name.
func check(t *testing.T, dir string, files map[string][]byte, root string) {
	t.Helper()
	if len(files) == 0 {
		t.Fatal("nothing was written")
	}
	want := filepath.Join("testdata", dir)
	if *update {
		if err := os.RemoveAll(want); err != nil {
			t.Fatal(err)
		}
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		got := files[name]
		if root != "" {
			got = bytes.ReplaceAll(got, []byte(root), []byte("$ROOT"))
		}
		path := filepath.Join(want, filepath.FromSlash(name))
		if *update {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: no snapshot (%v); a new file is a release decision: accept it with -update", name, err)
			continue
		}
		if !bytes.Equal(body, got) {
			t.Errorf("%s changed:\n--- snapshot\n%s\n--- now\n%s", name, body, got)
		}
	}
	if *update {
		return
	}
	_ = filepath.WalkDir(want, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(want, path)
		if _, ok := files[filepath.ToSlash(rel)]; !ok && !strings.HasPrefix(filepath.Base(rel), ".") {
			t.Errorf("%s is no longer written", filepath.ToSlash(rel))
		}
		return nil
	})
}
