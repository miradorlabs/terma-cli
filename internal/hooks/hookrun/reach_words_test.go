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

// wordsMachine is a folder holding a personal checkout, side, beside Acme's, acme-api,
// whose NOTES.md is there, and a home folder with a project in it; env is the hook's
// environment.
type wordsMachine struct {
	parent, side, api, notes, home string
	env                            map[string]string
}

func newWordsMachine(t *testing.T) wordsMachine {
	t.Helper()
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	m := wordsMachine{parent: parent, side: filepath.Join(parent, "side"), api: filepath.Join(parent, "acme-api"), home: filepath.Join(parent, "home")}
	m.notes = filepath.Join(m.api, "NOTES.md")
	for _, dir := range []string{filepath.Join(m.side, "src"), m.api, filepath.Join(m.home, "proj"), filepath.Join(parent, "tmp")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(m.notes, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m.env = map[string]string{"HOME": m.home, "TMPDIR": filepath.Join(parent, "tmp"), "ACME": m.api, "OLDPWD": parent}
	return m
}

// paths is ShellPaths in m, UnknownReach without the command it names.
func (m wordsMachine) paths(command, cwd string) []string {
	got := shellPaths(command, cwd, func(k string) (string, bool) { v, ok := m.env[k]; return v, ok })
	for i, p := range got {
		if isUnknownReach(p) {
			got[i] = UnknownReach
		}
	}
	return got
}

func (m wordsMachine) check(t *testing.T, cwd string, cases map[string][]string) {
	t.Helper()
	for command, want := range cases {
		if got := m.paths(command, cwd); !slices.Equal(got, want) {
			t.Errorf("ShellPaths(%q) = %q, want %q", command, got, want)
		}
	}
}

// A word without a slash is a path when something is there, from the cwd or where a cd
// went: git -C sibling, a search of a folder named alone. A command's name is not: the
// shell looks it up on PATH. One with nothing there names nothing.
func TestShellPathsTakeBareWordsThatNameSomethingThere(t *testing.T) {
	t.Parallel()
	m := newWordsMachine(t)
	m.check(t, m.parent, map[string][]string{
		"git -C acme-api log":     {m.api},
		"rg foo acme-api":         {m.api},
		"acme-api --help":         nil,
		"echo nothing-here":       nil,
		"ls side; acme-api":       {m.side},
		"time acme-api x":         nil,
		"X=1 acme-api":            nil,
		"cat < acme-api/NOTES.md": {m.api},
	})
	m.check(t, m.side, map[string][]string{
		"cd .. && rg foo acme-api": {m.parent, m.api},
		"cd ..; git -C acme-api":   {m.parent, m.api},
	})
}

// A cd's target may be a substitution: its words, which come first, are read as words of
// their own, and the cd goes where the word holding them says, $(pwd) or the top level
// included, or somewhere not known. A cd with no target, in a substitution or out of it,
// goes home.
func TestShellPathsReadACdToASubstitution(t *testing.T) {
	t.Parallel()
	m := newWordsMachine(t)
	m.check(t, m.side, map[string][]string{
		`cd "$(pwd)/../acme-api" && cat NOTES.md`: {m.api},
		"cd `pwd`/../acme-api; ls":                {m.api},
		`echo "$(cd)" && ls proj`:                 {m.home, filepath.Join(m.home, "proj")},
		`cd "$(cd ..; pwd)"`:                      {m.parent, UnknownReach},
	})
}

// The word in command position is the program to run, not a place the command reaches,
// even with a slash or a part not resolved: the first word, each after ;, && or |, and each
// after a word that keeps the command position (time, if, do). Its arguments are read as
// usual, and an assignment before it counts as any word does.
func TestShellPathsNeverCountTheCommandWord(t *testing.T) {
	t.Parallel()
	m := newWordsMachine(t)
	m.check(t, m.side, map[string][]string{
		"$(go env GOPATH)/bin/golangci-lint run":                       nil,
		"$UNSET/bin/tool && `which go`/../x run":                       nil,
		"../acme-api/bin/tool --version":                               nil,
		"time $(go env GOPATH)/bin/x ../acme-api":                      {m.api},
		"ls src | $(go env GOPATH)/bin/x":                              {filepath.Join(m.side, "src")},
		"D=../acme-api $(go env GOPATH)/bin/x":                         {m.api},
		"if $(go env GOPATH)/bin/x; then cat ../acme-api/NOTES.md; fi": {m.api},
	})
	if got := m.paths("$(go env GOPATH)/bin/x $(git rev-parse --show-prefix)/y", m.side); !slices.Equal(got, []string{UnknownReach}) {
		t.Errorf("an argument not resolved: %q, want UnknownReach", got)
	}
}

// Words are expanded before they are read: $HOME and ${HOME}, $PWD and $(pwd) and `pwd` where
// the command is, $OLDPWD where it was, $TMPDIR, any other variable the hook's environment
// sets, and an assignment earlier in the command, whose value is a path too.
func TestShellPathsExpandVariables(t *testing.T) {
	t.Parallel()
	m := newWordsMachine(t)
	m.check(t, m.side, map[string][]string{
		"ls $HOME/proj":                              {filepath.Join(m.home, "proj")},
		"ls ${HOME}/proj":                            {filepath.Join(m.home, "proj")},
		"ls ~/proj":                                  {filepath.Join(m.home, "proj")},
		"cat $ACME/NOTES.md":                         {m.api},
		`cat "${ACME}/NOTES.md"`:                     {m.api},
		"ls $ACME":                                   {m.api},
		"cd .. && cat $PWD/acme-api/NOTES.md":        {m.parent, m.api},
		"cat $(pwd)/../acme-api/NOTES.md":            {m.api},
		"cat `pwd`/../acme-api/NOTES.md":             {m.api},
		`cat "$(pwd -P)/../acme-api/NOTES.md"`:       {m.api},
		"ls $OLDPWD/acme-api":                        {m.api},
		"cd ../acme-api && cd ../side && ls $OLDPWD": {m.api, m.side},
		"echo x > $TMPDIR/out":                       {filepath.Join(m.parent, "tmp")},
		"D=../acme-api; ls $D":                       {m.api},
		"D=../acme-api && cat $D/NOTES.md":           {m.api},
		"ls ${UNSET:-../acme-api}":                   {m.api},
		"cd ../acme-api && cd - && ls":               {m.api},
		"cd ../acme-api; cd; ls proj":                {m.api, m.home, filepath.Join(m.home, "proj")},
	})
}

// A word that may be a path, holding a slash or a cd's target, whose expansion cannot be
// resolved reaches somewhere not known: a command substitution's output (the top level
// outside any checkout among them), an unset variable, a positional parameter, or an
// assignment from one of them.
func TestShellPathsCannotResolveIsUnknown(t *testing.T) {
	t.Parallel()
	m := newWordsMachine(t)
	for _, command := range []string{
		"cat $UNSET/NOTES.md",
		`cat "${UNSET}/x"`,
		"cd $UNSET && ls",
		"cat $(git rev-parse --show-prefix)/NOTES.md",
		"cat `git rev-parse --show-prefix`/NOTES.md",
		"cat $(git rev-parse --show-toplevel)/NOTES.md",
		"cat $1/NOTES.md",
		"D=$(git rev-parse --show-prefix); cat $D/x",
		"cat ${ACME%/*}/x",
		"ls --dir=$UNSET/x",
		`cd "$(cat p)" && cat NOTES.md`,
		"pushd `cat p`; cat NOTES.md",
		`cd -P "$(dirname "$(cat p)")" && ls`,
		`x=$(cd "$(cat p)" && pwd)`,
	} {
		if got := m.paths(command, m.side); !slices.Contains(got, UnknownReach) {
			t.Errorf("ShellPaths(%q) = %q, without UnknownReach", command, got)
		}
	}
	for _, command := range []string{"echo $UNSET", "git commit -m \"$(date)\"", "echo '$UNSET/x'"} {
		if got := m.paths(command, m.side); slices.Contains(got, UnknownReach) {
			t.Errorf("ShellPaths(%q) = %q: a word that is no path is not unknown", command, got)
		}
	}
}

// A word with a live wildcard is what it matches, each match reaching its folder, or a
// matched folder itself; a quoted wildcard is taken as written, and a pattern that matches
// nothing is passed on as written. However many files a glob matches in one folder, it
// reaches one place: only matches in more than maxShellPlaces folders reach somewhere not
// known.
func TestShellPathsExpandGlobs(t *testing.T) {
	t.Parallel()
	m := newWordsMachine(t)
	m.check(t, m.side, map[string][]string{
		"cat ../acme-*/NOTES.md":   {m.api},
		"ls ../acme-a?i":           {m.api},
		"ls ../acme-[a]pi":         {m.api},
		"cat '../acme-*/NOTES.md'": {m.parent},
		`cat ../acme-\*/NOTES.md`:  {m.parent},
		"cat ../nothing-*/x":       {m.parent},
		"cat $ACME/*.md":           {m.api},
	})
	many := filepath.Join(m.parent, "many")
	for i := range maxShellPlaces + 1 {
		for _, rel := range []string{fmt.Sprintf("f%03d.go", i), fmt.Sprintf("d%03d/x.go", i)} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(many, rel)), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(many, rel), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	m.check(t, many, map[string][]string{"gofmt -l *.go": {many}})
	if got := m.paths("ls d*/*.go", many); !slices.Contains(got, UnknownReach) || len(got) != maxShellPlaces+1 {
		t.Errorf("a glob past the bound: %d places, UnknownReach %v", len(got), slices.Contains(got, UnknownReach))
	}
}

// The scan makes at most maxShellLookups lookups, a word's or a glob match's each: a command
// past them reaches somewhere not known, however few places it names.
func TestShellPathsPastTheLookupBoundIsUnknown(t *testing.T) {
	t.Parallel()
	m := newWordsMachine(t)
	words := func(n int) string {
		var b strings.Builder
		b.WriteString("echo")
		for i := range n {
			fmt.Fprintf(&b, " word%d", i)
		}
		return b.String()
	}
	if got := m.paths(words(maxShellLookups), m.side); got != nil {
		t.Errorf("%d words that name nothing: %q", maxShellLookups, got)
	}
	if got := m.paths(words(maxShellLookups+1), m.side); !slices.Equal(got, []string{UnknownReach}) {
		t.Errorf("%d words that name nothing: %q, want UnknownReach", maxShellLookups+1, got)
	}
}

// A here-document's body is data unless its command is a shell: only its slashed words may
// be paths there, so a commit message's words are not candidates, and one that names
// nothing names no new file either; a shell's body is commands. A quoted delimiter's body
// expands nothing.
func TestShellPathsReadHereDocuments(t *testing.T) {
	t.Parallel()
	m := newWordsMachine(t)
	m.check(t, m.parent, map[string][]string{
		"git commit -F - <<'EOF'\nfix acme-api and side\nEOF":   nil,
		"git commit -m \"$(cat <<'EOF'\nfix acme-api\nEOF\n)\"": nil,
		"bash <<'EOF'\ncd acme-api\nEOF":                        {m.api},
		"cat <<'EOF' | bash\ncat acme-api/NOTES.md\nEOF":        {m.api},
		"cat <<EOF\n$(cat acme-api/NOTES.md)\nEOF":              {m.api},
		"cat <<'EOF'\n$UNSET/x\nEOF":                            nil,
		"cat <<'EOF'\nsee acme-api/NOTES.md\nEOF":               {m.api},
		"cat <<'EOF'\nsee acme-api/new/x.md and a/b\nEOF":       nil,
		"cat <<-EOF > side/out\n\tacme-api\n\tEOF\nls side/src": {m.side, filepath.Join(m.side, "src")},
	})
}

// A for loop's variable is each of its words in turn, its body read once for each: a
// loop over packages reaches each package's folder, and one over checkouts each checkout.
// With no in, or over words not resolved, the variable is not known. Every pass is a
// lookup, so loops nested past maxShellLookups passes reach somewhere not known.
func TestShellPathsBindAForLoopsVariable(t *testing.T) {
	t.Parallel()
	m := newWordsMachine(t)
	src := filepath.Join(m.side, "src")
	m.check(t, m.side, map[string][]string{
		"for d in ../acme-api ../side; do make -C $d; done":            {m.api, m.side},
		"for pkg in a b; do go test ./src/$pkg/...; done":              {src},
		"for f in NOTES.md; do cat ../acme-api/$f; done":               {m.api},
		"for f in NOTES.md\ndo\n  cat ../acme-api/$f\ndone":            {m.api},
		"for a in x y; do for b in 1 2; do echo src/$a/$b; done; done": {src},
		"for d in a; do while true; do ls src/$d; done; done":          {src},
		"for f in a; do echo $f; done; cat ../acme-api/$f/../NOTES.md": {m.api},
	})
	for _, command := range []string{"for x; do cat $x/y; done", "for d in $(ls); do cat $d/x; done", "for d in a; do cd $UNSET; done"} {
		if got := m.paths(command, m.side); !slices.Contains(got, UnknownReach) {
			t.Errorf("ShellPaths(%q) = %q, without UnknownReach", command, got)
		}
	}
	many := strings.TrimSpace(strings.Repeat("w ", 70))
	if got := m.paths("for a in "+many+"; do for b in "+many+"; do echo $a$b; done; done", m.side); !slices.Equal(got, []string{UnknownReach}) {
		t.Errorf("70 by 70 passes: %q, want UnknownReach", got)
	}
}

// $(git rev-parse --show-toplevel) is the root of the checkout the command is in when it
// runs, where an earlier cd went included.
func TestShellPathsResolveTheGitTopLevel(t *testing.T) {
	t.Parallel()
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	side, api := hookruntest.RepoIn(t, parent, "github.com/dawson/side"), hookruntest.RepoIn(t, parent, "github.com/acme/acme-api")
	hookruntest.WriteFile(t, side, "internal/x.go", "package internal\n")
	hookruntest.WriteFile(t, api, "NOTES.md", "acme\n")
	in := filepath.Join(side, "internal")
	for command, want := range map[string][]string{
		"cat $(git rev-parse --show-toplevel)/go.mod":                       {side},
		`cat "$(git rev-parse --show-toplevel)/../acme-api/NOTES.md"`:       {api},
		"cd ../../acme-api && cat `git rev-parse --show-toplevel`/NOTES.md": {api},
		"D=$(git  rev-parse  --show-toplevel)/internal; ls $D":              {in},
	} {
		if got := ShellPaths(command, in); !sameReach(got, want) {
			t.Errorf("ShellPaths(%q) = %q, want %q", command, got, want)
		}
	}
}

// UnknownReach names the command it stands for by its first four words, at most 60
// characters, for doctor: an assignment's or an option's value is left out.
func TestUnknownReachNamesTheCommandByItsFirstWords(t *testing.T) {
	t.Parallel()
	for command, want := range map[string]string{
		`cd "$(cat p)" && cat NOTES.md`:                 `cd "$(cat p)" && …`,
		"cat $UNSET/x":                                  "cat $UNSET/x",
		"TOKEN=s3cret curl --header=s3cret $UNSET/x -v": "TOKEN=… curl --header=… $UNSET/x …",
		"cat /" + strings.Repeat("x", 80) + "/$UNSET/y": "cat /" + strings.Repeat("x", 55) + " …",
	} {
		got := ShellPaths(command, t.TempDir())
		if len(got) == 0 || got[len(got)-1] != UnknownReach+"\x00"+want {
			t.Errorf("ShellPaths(%q) = %q, want UnknownReach naming %q", command, got, want)
		}
	}
}

// Accepted risk: a word with no slash whose expansion cannot be resolved counts for
// nothing, so a loop's variable naming another organization's checkout is missed. Taking
// every such word for a reach somewhere unknown would turn every for loop Mixed.
func TestShellPathsMissAnUnresolvedWordWithNoSlash(t *testing.T) {
	t.Parallel()
	m := newWordsMachine(t)
	m.check(t, m.parent, map[string][]string{
		"for d in $(ls); do rg TODO $d; done": nil,
		"rg TODO $DIR":                        nil,
	})
}

// A path that is or passes through a symlink reaches its target's checkout as well as its
// own: a link to Acme's checkout, or to a file in it, from a personal checkout crosses.
func TestASymlinkReachesItsTarget(t *testing.T) {
	t.Parallel()
	side, api := hookruntest.RepoAt(t, "github.com/dawson/side"), hookruntest.RepoAt(t, "github.com/acme/api")
	hookruntest.WriteFile(t, api, "NOTES.md", "acme\n")
	if err := os.Symlink(api, filepath.Join(side, "vendor")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if err := os.Symlink(filepath.Join(api, "NOTES.md"), filepath.Join(side, "notes.md")); err != nil {
		t.Fatal(err)
	}
	// A link into a folder of Acme's checkout: walking up from the link finds side's .git.
	hookruntest.WriteFile(t, api, "src/main.go", "package main\n")
	if err := os.Symlink(filepath.Join(api, "src"), filepath.Join(side, "acme-src")); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"cat vendor/NOTES.md", "cat ./notes.md", "cat notes.md", "ls vendor", "cd vendor && ls",
		"cat ./acme-src/main.go", "ls ./acme-src", "cd acme-src && ls"} {
		t.Run(strings.ReplaceAll(command, "/", "_"), func(t *testing.T) {
			t.Parallel()
			m := newRoutedMachine(t, acmeAndPersonalRepos(t))
			const sid = "linked-session"
			r := startIn(t, m.hook(), sid, side)
			env := m.hook()
			env.Cwd = side
			env.Classify(context.Background(), r, sessionOf(sid), ShellPaths(command, side), nil)
			if ledger, placement := m.mixed(sid); !ledger || !placement {
				t.Fatalf("ledger Mixed %v, mixed placement %v", ledger, placement)
			}
		})
	}
	m := newRoutedMachine(t, acmeAndPersonalRepos(t))
	const sid = "own-link-session"
	r := startIn(t, m.hook(), sid, side)
	if err := os.Symlink(filepath.Join(side, "README"), filepath.Join(side, "readme.md")); err != nil {
		t.Fatal(err)
	}
	env := m.hook()
	env.Cwd = side
	env.Classify(context.Background(), r, sessionOf(sid), ShellPaths("cat readme.md", side), nil)
	if ledger, _ := m.mixed(sid); ledger {
		t.Fatal("a link within its own checkout made the run Mixed")
	}
}
