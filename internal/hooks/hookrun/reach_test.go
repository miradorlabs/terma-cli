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
//
// Some words are not paths whole: those inside a command substitution, $(...) or `...`,
// quoted or not, and a subshell's, (...); an option's or an assignment's value (--flag=X,
// -oX, NAME=X); and a relative path after a cd or a pushd, which resolves against where
// that went as well as the cwd.
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

		"echo $(cat '../acme api/NOTES.md')":          {notes},
		"echo `cat ../acme\\ api/NOTES.md`":           {notes},
		`echo "$(cat '../acme api/NOTES.md')"`:        {notes},
		`echo "notes: $(cat "../acme api/NOTES.md")"`: {notes},
		`x="$(cd '../acme api' && pwd)"`:              {api},
		"echo '$(cat ../acme\\ api/NOTES.md)'":        nil,
		"(cd '../acme api')":                          {api},
		"(cat '../acme api/NOTES.md')":                {notes},
		"grep --file='../acme api/NOTES.md' TODO":     {notes},
		"cc -I'../acme api' -o out.o x.c":             {api},
		"cc -o'../acme api/NOTES.md' x.c":             {notes},
		"GIT_DIR='../acme api' git status":            {api},
		"pushd src && ls":                             {filepath.Join(cwd, "src")},
		"cd -P src":                                   {filepath.Join(cwd, "src")},
		"cd .. && cat 'acme api/NOTES.md'":            {parent, notes},
		"(cd src) && cat '../acme api/NOTES.md'":      {filepath.Join(cwd, "src"), notes},
	} {
		if got := ShellPaths(command, cwd); !slices.Equal(got, want) {
			t.Errorf("ShellPaths(%q) = %q, want %q", command, got, want)
		}
	}
}

// Only a command's first maxShellPaths candidates are examined, as the accepted risk on
// ShellPaths says: Acme's checkout among the first turns the run Mixed however long the
// command, and one only past the bound leaves the run as it was.
func TestAShellCommandIsExaminedUpToTheCandidateBound(t *testing.T) {
	t.Parallel()
	st := acmeAndPersonalRepos(t)
	side, api := hookruntest.RepoAt(t, "github.com/dawson/side"), hookruntest.RepoAt(t, "github.com/acme/api")
	for _, tc := range []struct {
		candidates int
		acmeAt     int // Acme's position among the candidates, from 0
		mixed      bool
	}{{maxShellPaths, 0, true}, {maxShellPaths + 5, 0, true}, {maxShellPaths + 5, maxShellPaths, false}} {
		t.Run(fmt.Sprint(tc.candidates), func(t *testing.T) {
			t.Parallel()
			m := newRoutedMachine(t, st)
			sid := fmt.Sprintf("long-command-%d-%d", tc.candidates, tc.acmeAt)
			r := startIn(t, m.hook(), sid, side)
			words := []string{"ls"}
			for i := range tc.candidates {
				if i == tc.acmeAt {
					words = append(words, api)
				} else {
					words = append(words, fmt.Sprintf("src/missing-%d.go", i))
				}
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
