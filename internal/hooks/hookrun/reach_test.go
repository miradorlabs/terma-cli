package hookrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// A shell command's paths are its absolute, ~/ and slashed words and every cd's target,
// split at whitespace and the shell's operators outside quotes, resolved against the cwd,
// and kept where something is there. A word with nothing there may be a file the command
// creates, so it reaches the nearest folder that is there: another checkout's for a new
// file there, the cwd's own for a word that only looks like a path. A cd to nowhere goes
// nowhere.
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
		"cat '../acme api/MISSING.md'":       {api},
		"curl https://example.com/a/b > out": {cwd},
		"echo cd && true":                    nil,

		"echo x > '../acme api/new.md'":         {api},
		"mkdir -p '../acme api/a/b'":            {api},
		"echo x > src/new.go":                   {filepath.Join(cwd, "src")},
		"cd '../acme api/nowhere' && ls":        nil,
		"cd .. && echo x > 'acme api/new.md'":   {parent, api, cwd},
		"cd src && echo x > '../src/new.go'":    {filepath.Join(cwd, "src"), parent},
		"echo x > '" + notes + "/not-a-folder'": nil,

		"echo $(cat '../acme api/NOTES.md')":          {notes},
		"echo `cat ../acme\\ api/NOTES.md`":           {notes},
		`echo "$(cat '../acme api/NOTES.md')"`:        {notes},
		`echo "notes: $(cat "../acme api/NOTES.md")"`: {notes},
		`x="$(cd '../acme api' && pwd)"`:              {api},
		"echo '$(cat ../acme\\ api/NOTES.md)'":        {cwd},
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

// A command's first maxShellPaths candidates are examined, and one with more counts as
// reaching an unknown organization: Acme's checkout among the first turns the run Mixed,
// and so does any command past the bound, though none of its candidates names another
// checkout.
func TestAShellCommandPastTheCandidateBoundTurnsTheRunMixed(t *testing.T) {
	t.Parallel()
	st := acmeAndPersonalRepos(t)
	side, api := hookruntest.RepoAt(t, "github.com/dawson/side"), hookruntest.RepoAt(t, "github.com/acme/api")
	for _, tc := range []struct {
		candidates int
		acmeAt     int // Acme's position among the candidates, from 0; -1 for none
		mixed      bool
	}{{maxShellPaths, 0, true}, {maxShellPaths, -1, false}, {maxShellPaths + 1, -1, true}, {maxShellPaths + 5, maxShellPaths, true}} {
		t.Run(fmt.Sprint(tc.candidates, tc.acmeAt), func(t *testing.T) {
			t.Parallel()
			m := newRoutedMachine(t, st)
			sid := fmt.Sprintf("long-command-%d-%d", tc.candidates, tc.acmeAt)
			r := startIn(t, m.hook(), sid, side)
			words := []string{"ls"}
			for i := range tc.candidates {
				if i == tc.acmeAt {
					// Quoted, as a shell needs a Windows path's backslashes.
					words = append(words, "'"+api+"'")
				} else {
					words = append(words, fmt.Sprintf("src/missing-%d.go", i))
				}
			}
			paths := ShellPaths(strings.Join(words, " "), side)
			if past := slices.Contains(paths, UnknownReach); past != (tc.candidates > maxShellPaths) {
				t.Fatalf("ShellPaths named UnknownReach %v for %d candidates", past, tc.candidates)
			}
			env := m.hook()
			env.Cwd = side
			env.Classify(context.Background(), r, sessionOf(sid), paths, nil)
			if ledger, placement := m.mixed(sid); ledger != tc.mixed || placement != tc.mixed {
				t.Fatalf("ledger Mixed %v, mixed placement %v; want %v", ledger, placement, tc.mixed)
			}
		})
	}
}

// A hook whose own checkout cannot be located, its cwd deleted, still classifies what the
// call reached against where the run has been: a read of Acme's checkout by a run placed
// in personal's turns it Mixed. A reach that crosses nothing writes no placement.
func TestAHookThatCannotLocateItsCheckoutStillClassifiesWhatItReached(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		origin string
		mixed  bool
	}{
		"another organization's":  {"github.com/acme/api", true},
		"the same organization's": {"github.com/dawson/web", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newRoutedMachine(t, acmeAndPersonal(t))
			side, other := hookruntest.RepoAt(t, "github.com/dawson/side"), hookruntest.RepoAt(t, tc.origin)
			const sid = "deleted-cwd"
			startIn(t, m.hook(), sid, side)
			before := m.claim(sid)
			env := m.hook()
			env.Cwd = filepath.Join(t.TempDir(), "deleted")
			if _, err := env.routeHere(context.Background()); err == nil {
				t.Fatal("the deleted cwd was located")
			}
			env.Classify(context.Background(), nil, sessionOf(sid), []string{filepath.Join(other, "README")}, nil)
			if ledger, placement := m.mixed(sid); ledger != tc.mixed || placement != tc.mixed {
				t.Fatalf("ledger Mixed %v, mixed placement %v; want %v", ledger, placement, tc.mixed)
			}
			if c := m.claim(sid); !tc.mixed && !slices.EqualFunc(c.Placements, before.Placements, func(a, b claim.Placement) bool { return a.Kind == b.Kind && a.Repository == b.Repository }) {
				t.Errorf("a hook that located nothing placed the session: %+v", c.Placements)
			}
		})
	}
}

// A hook that classifies before it records locates its checkout once: RepoAt takes the
// checkout Classify located and gives it its session store, and refuses one Route drops.
func TestRepoAtTakesTheCheckoutClassifyLocated(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t, acmeAndPersonalRepos(t))
	ctx := context.Background()
	for origin, collected := range map[string]bool{"github.com/dawson/side": true, "github.com/other/lib": false} {
		root := hookruntest.RepoAt(t, origin)
		env := m.hook()
		env.Cwd = root
		located := env.Classify(ctx, nil, sessionOf("located-once"), []string{filepath.Join(root, "README")}, nil)
		if located == nil || located.Store != nil {
			t.Fatalf("%s: Classify located %+v", origin, located)
		}
		r, err := env.RepoAt(ctx, located)
		switch {
		case collected && (err != nil || r != located || r.Store == nil):
			t.Errorf("%s: RepoAt = %+v, %v; want the located checkout with its store", origin, r, err)
		case !collected && !errors.Is(err, ErrNotAdmitted):
			t.Errorf("%s: RepoAt = %+v, %v; want ErrNotAdmitted", origin, r, err)
		}
	}
	env := m.hook()
	if got := env.Classify(ctx, nil, sessionOf("located-once"), nil, nil); got != nil {
		t.Errorf("Classify with nothing to classify located %+v", got)
	}
}
