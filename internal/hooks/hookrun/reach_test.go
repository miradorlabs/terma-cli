package hookrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
)

// A shell command's paths are its absolute, ~/ and slashed words and every cd's target,
// split at whitespace and the shell's operators outside quotes, resolved against the cwd,
// and kept only where something is there.
func TestShellPathsAreTheWordsThatNameSomethingThere(t *testing.T) {
	t.Parallel()
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	cwd, api := filepath.Join(parent, "side"), filepath.Join(parent, "acme api")
	for _, dir := range []string{filepath.Join(cwd, "src"), api} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	notes := filepath.Join(api, "NOTES.md")
	if err := os.WriteFile(notes, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for command, want := range map[string][]string{
		"echo x >> '" + notes + "'":          {notes},
		`cd "../acme api"&&ls`:               {api},
		`cd ../acme\ api || ls src/`:         {api, filepath.Join(cwd, "src")},
		"cat <'../acme api/NOTES.md'|wc -l":  {notes},
		"ls -la":                             nil,
		"cd src; ls":                         {filepath.Join(cwd, "src")},
		"cat '../acme api/MISSING.md'":       nil,
		"curl https://example.com/a/b > out": nil,
		"echo cd && true":                    nil,
	} {
		if got := ShellPaths(command, cwd); !slices.Equal(got, want) {
			t.Errorf("ShellPaths(%q) = %q, want %q", command, got, want)
		}
	}
}

// Past maxShellPaths candidates a command is not examined at all, as the accepted risk on
// ShellPaths says: one that reaches Acme's checkout among more candidates than that leaves
// the run as it was, while one at the bound turns it Mixed.
func TestAShellCommandPastTheCandidateBoundIsNotExamined(t *testing.T) {
	t.Parallel()
	st := acmeAndPersonalRepos(t)
	side, api := hookruntest.RepoAt(t, "github.com/dawson/side"), hookruntest.RepoAt(t, "github.com/acme/api")
	for _, tc := range []struct {
		candidates int
		mixed      bool
	}{{maxShellPaths, true}, {maxShellPaths + 1, false}} {
		t.Run(fmt.Sprint(tc.candidates), func(t *testing.T) {
			t.Parallel()
			m := newRoutedMachine(t, st)
			sid := fmt.Sprintf("long-command-%d", tc.candidates)
			r := startIn(t, m.hook(), sid, side)
			words := []string{"ls", api}
			for i := 1; i < tc.candidates; i++ {
				words = append(words, fmt.Sprintf("src/missing-%d.go", i))
			}
			env := m.hook()
			env.Cwd = side
			env.Classify(context.Background(), r, sessionOf(sid), ShellPaths(strings.Join(words, " "), side), nil)
			if ledger, placement := m.mixed(sid); ledger != tc.mixed || placement != tc.mixed {
				t.Fatalf("ledger Mixed %v, mixed placement %v; want %v", ledger, placement, tc.mixed)
			}
		})
	}
}
