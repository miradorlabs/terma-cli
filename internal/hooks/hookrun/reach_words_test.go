package hookrun

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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

// paths is ShellPaths in m, UnknownReach without the command it names, given a minute: these
// tests hold the scan to its other bounds, which a scan under a loaded test run may take
// longer than maxShellTime to reach (TestShellPathsEndAtTheirDeadline holds it to a hook's).
func (m wordsMachine) paths(command, cwd string) []string {
	got := shellPathsBy(command, cwd, func(k string) (string, bool) { v, ok := m.env[k]; return v, ok }, time.Now().Add(time.Minute))
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
		`cd "$(pwd)/../acme-api" && cat NOTES.md`:                         {m.api},
		"cd `pwd`/../acme-api; ls":                                        {m.api},
		`echo "$(cd && ls proj)"`:                                         {m.home, filepath.Join(m.home, "proj")},
		`echo "$(cd)" && ls proj`:                                         {m.home},
		`cd ..; x=$(cd side); cat acme-api/NOTES.md`:                      {m.parent, m.side, m.api},
		`cd "$(cd ..; pwd)"`:                                              {m.parent, UnknownReach},
		`if true; then D=acme-api; else D=side; fi; cat ../$D/NOTES.md`:   {UnknownReach},
		`if true; then cd ../acme-api; else cd .; fi; cat NOTES.md`:       {m.api, m.side, UnknownReach},
		`case $X in a) D=acme-api;; b) D=side;; esac; cat ../$D/NOTES.md`: {UnknownReach},
		`x=$(cat p); cat "$x"`:                                            {UnknownReach},
		`if true; then D=../acme-api; else D=side; fi; cat "$D"`:          {m.api, UnknownReach},
		`builtin export CDPATH=..; cd acme-api && cat NOTES.md`:           {m.api},
	})
}

// The word in command position is the program to run: a place the command reaches only
// when it is a path that is there, never a word the shell looks up on PATH, one not
// resolved, or a file not there, which the command cannot create. It is the first word,
// each after ;, && or |, and each after a word that runs the next (time, nohup, exec). Its
// arguments are read as usual, and an assignment before it counts as any word does.
func TestShellPathsCountTheCommandWordOnlyAsAPathThere(t *testing.T) {
	t.Parallel()
	m := newWordsMachine(t)
	m.check(t, m.side, map[string][]string{
		"$(go env GOPATH)/bin/golangci-lint run":                       nil,
		"$UNSET/bin/tool && `which go`/../x run":                       nil,
		"../acme-api/bin/tool --version":                               nil,
		"../acme-api/NOTES.md --version":                               {m.api},
		"nohup ../acme-api/NOTES.md":                                   {m.api},
		"ls src | ../acme-api/NOTES.md":                                {filepath.Join(m.side, "src"), m.api},
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
		"cat ${ACME%/*}/acme-api/NOTES.md":           {m.api},
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
		"cat ${UNSET%/*}/x",
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
		"ls ~/pr*":                 {filepath.Join(m.home, "proj")},
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

// Each of the scan's other bounds makes the reach not known past it: the words it expands,
// the bytes it parses, the scripts it reads inside each other, the segments of a glob that
// hold a wildcard, and the entries a glob reads. A command that does not parse is still
// read, as its words.
func TestShellPathsPastTheirOtherBoundsAreUnknown(t *testing.T) {
	t.Parallel()
	m := newWordsMachine(t)
	big := filepath.Join(m.parent, "big")
	if err := os.MkdirAll(big, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range maxShellLookups {
		if err := os.WriteFile(filepath.Join(big, fmt.Sprint(i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// nest runs command in a shell, quoted, depth times.
	nest := func(command string, depth int) string {
		for range depth {
			command = "sh -c '" + strings.ReplaceAll(command, "'", `'\''`) + "'"
		}
		return command
	}
	for command, unknown := range map[string]bool{
		"echo" + strings.Repeat(" $U", maxShellWords-1):   false,
		"echo" + strings.Repeat(" $U", maxShellWords):     true,
		"echo " + strings.Repeat("x", maxShellBytes-5):    false,
		"echo " + strings.Repeat("x", maxShellBytes):      true,
		nest("cat ../acme-api/NOTES.md", maxShellDepth-1): false,
		nest("cat ../acme-api/NOTES.md", maxShellDepth):   true,
		"ls src/*/*/*":   false,
		"ls src/*/*/*/*": true,
		"ls ../big/*":    true,
	} {
		if got := m.paths(command, m.side); slices.Contains(got, UnknownReach) != unknown {
			t.Errorf("ShellPaths(%.60q…) = %q, want UnknownReach %v", command, got, unknown)
		}
	}
	m.check(t, m.side, map[string][]string{
		`cat "../acme-api/NOTES.md`:                           {m.api},
		`(cat $ACME/NOTES.md`:                                 {m.api},
		nest("cat ../acme-api/NOTES.md", maxShellDepth-1):     {m.api},
		"for a in 1 2 3; do for b in 1 2 3; do :; done; done": nil,
	})
}

// A shell's script is read as commands, in a subshell: bash -c's, eval's, trap's, and a
// here-string's or a here-document's fed to a shell; an interpreter's (python -c, node -e) for its slashed
// words. A shell's script not resolved may do anything. A folder option's value (-C for
// git, make, tar and env; --git-dir, --dir and the like) not resolved is not known either,
// but another command's -C (grep's context, git diff's copies) is no folder, and pushd's
// move ends at popd.
func TestShellPathsReadInlineScriptsAndFolderOptions(t *testing.T) {
	t.Parallel()
	m := newWordsMachine(t)
	src := filepath.Join(m.side, "src")
	m.check(t, m.side, map[string][]string{
		"bash -lc 'cd ../acme-api && ls'; ls $PWD/src":    {m.api, src},
		"bash -ce 'cd .. && cat acme-api/NOTES.md'":       {m.parent, m.api},
		"bash -euxc 'cd .. && cat acme-api/NOTES.md'":     {m.parent, m.api},
		"timeout 60 bash -ec 'cat ../acme-api/NOTES.md'":  {m.api},
		"bash -o pipefail -c 'cd ../acme-api && ls'":      {m.api},
		"bash +o errexit -c 'cd ../acme-api && ls'":       {m.api},
		"{ echo 'cd ../acme-api && ls'; } | bash":         {m.side, m.api},
		"cat <(cat ../acme-api/NOTES.md)":                 {m.api},
		"cd src | true; cd ..; cat acme-api/NOTES.md":     {src, m.parent, m.api},
		"cd src & cd ..; cat acme-api/NOTES.md":           {src, m.parent, m.api},
		"D=../acme-api; D=src true; cat $D/NOTES.md":      {m.api},
		"D=../acme-api; (D=src); cat $D/NOTES.md":         {m.api},
		"D=../acme; D+=-api; cat $D/NOTES.md":             {m.parent, m.api},
		"pushd ../acme-api; pushd .; popd; cat NOTES.md":  {m.api, m.side},
		"a=( $(cat ../acme-api/NOTES.md) )":               {m.api},
		"(( n = $(wc -l < ../acme-api/NOTES.md) ))":       {m.api},
		"P=..; tar -C \"$P\" -czf out.tgz acme-api":       {m.parent, m.api},
		"env --chdir=.. cat acme-api/NOTES.md":            {m.parent, m.api},
		"uv run --directory .. cat acme-api/NOTES.md":     {m.parent, m.api},
		`bash <<< "cat ../acme-api/NOTES.md"`:             {m.api},
		`ruby -e "File.read('../acme-api/NOTES.md')"`:     {m.api},
		"perl -ne 'print if $. < 3' ../acme-api/NOTES.md": {m.api},
		"grep -C $N TODO src":                             {src},
		"eval 'cat ../acme-api/NOTES.md'":                 {m.api},
		"trap 'cat ../acme-api/NOTES.md' EXIT":            {m.api},
		"coproc cat ../acme-api/NOTES.md":                 {m.api},
		"git diff -C $(git merge-base HEAD main) -- src":  {src},
		"pushd ../acme-api && popd && ls $PWD/src":        {m.api, src},
		"cat <<'EOF' | grep x | sh\ncd ../acme-api\nEOF":  {m.api, UnknownReach},
		"bash < ../acme-api/NOTES.md":                     {m.api, UnknownReach},
	})
	m.env["CDPATH"] = m.parent
	if got := m.paths("cd acme-api && cat NOTES.md", m.side); !slices.Contains(got, m.api) {
		t.Errorf("CDPATH from env: %q, want %q", got, m.api)
	}
	delete(m.env, "CDPATH")
	for _, command := range []string{
		`bash -c "$CMD"`,
		`eval "$CMD"`,
		"git --git-dir=$(cat p) log",
		"tar -C $UNSET -xf x.tar",
		"env -C`cat p` make",
		"make --directory $(cat p)",
		"tar -xf x.tar -C $(cat p)",
		`cat <(cat "$(cat p)"/NOTES.md)`,
		"printf 'cd %s\\n' .. | sh",
	} {
		if got := m.paths(command, m.side); !slices.Contains(got, UnknownReach) {
			t.Errorf("ShellPaths(%q) = %q, without UnknownReach", command, got)
		}
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
	side := makeCheckoutDir(t, parent, "side")
	api := makeCheckoutDir(t, parent, "acme-api")
	writeCorpusFile(t, side, "internal/x.go", "package internal\n")
	writeCorpusFile(t, api, "NOTES.md", "acme\n")
	in := filepath.Join(side, "internal")
	for command, want := range map[string][]string{
		"cat $(git rev-parse --show-toplevel)/go.mod":                       {side},
		`cat "$(git rev-parse --show-toplevel)/../acme-api/NOTES.md"`:       {api},
		"cd ../../acme-api && cat `git rev-parse --show-toplevel`/NOTES.md": {api},
		"D=$(git  rev-parse  --show-toplevel)/internal; ls $D":              {in},
	} {
		if got := ShellPaths(command, in); !sameReach(command, got, want) {
			t.Errorf("ShellPaths(%q) = %q, want %q", command, got, want)
		}
	}
}

// UnknownReach names the command it stands for by its first four words, at most 60
// characters, for doctor: an assignment's or an option's value is left out, after its = or
// written against a one-letter option.
func TestUnknownReachNamesTheCommandByItsFirstWords(t *testing.T) {
	t.Parallel()
	for command, want := range map[string]string{
		`cd "$(cat p)" && cat NOTES.md`:                    `cd "$(cat p)" && cat …`,
		"cat $UNSET/x":                                     "cat $UNSET/x",
		"TOKEN=val1 curl --header=val2 $UNSET/x -v":        "TOKEN=… curl --header=… $UNSET/x …",
		"mysql -pval1 -h db < $UNSET/x":                    "mysql -p… -h db …",
		"curl -u user:val1 $UNSET/x":                       "curl -u … $UNSET/x",
		`curl -H "X-Custom: val1" $UNSET/x`:                "curl -H … $UNSET/x",
		"tool --api-key val1 $UNSET/x":                     "tool --api-key … $UNSET/x",
		"git clone https://me:val1@example.com/x $UNSET/y": "git clone https://…@example.com/x $UNSET/y",
		"cat /" + strings.Repeat("x", 80) + "/$UNSET/y":    "cat /" + strings.Repeat("x", 55) + " …",
		"redis-cli -a val1 --rdb $UNSET/x":                 "redis-cli -a … --rdb …",
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
// own: a link to Acme's checkout, or to a file in it, from a personal checkout reaches Acme.
func TestASymlinkReachesItsTarget(t *testing.T) {
	t.Parallel()
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	side := makeCheckoutDir(t, parent, "side")
	api := makeCheckoutDir(t, parent, "acme-api")
	writeCorpusFile(t, side, "README", "side\n")
	writeCorpusFile(t, api, "NOTES.md", "acme\n")
	writeCorpusFile(t, api, "src/main.go", "package main\n")
	if err := os.Symlink(api, filepath.Join(side, "vendor")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if err := os.Symlink(filepath.Join(api, "NOTES.md"), filepath.Join(side, "notes.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(api, "src"), filepath.Join(side, "acme-src")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		command string
		want    string
	}{
		{"cat vendor/NOTES.md", api},
		{"cat ./notes.md", api},
		{"cat notes.md", api},
		{"ls vendor", api},
		{"cd vendor && ls", api},
		{"cat ./acme-src/main.go", filepath.Join(api, "src")},
		{"ls ./acme-src", filepath.Join(api, "src")},
		{"cd acme-src && ls", filepath.Join(api, "src")},
	} {
		got := ShellPaths(tc.command, side)
		reached := slices.ContainsFunc(got, func(p string) bool {
			if p == tc.want {
				return true
			}
			resolved, err := filepath.EvalSymlinks(p)
			return err == nil && resolved == tc.want
		})
		if !reached {
			t.Errorf("ShellPaths(%q) = %q, want %q (or symlink to it) among reached places", tc.command, got, tc.want)
		}
	}
	if err := os.Symlink(filepath.Join(side, "README"), filepath.Join(side, "readme.md")); err != nil {
		t.Fatal(err)
	}
	got := ShellPaths("cat readme.md", side)
	if slices.Contains(got, api) || slices.ContainsFunc(got, isUnknownReach) || !slices.Contains(got, side) {
		t.Errorf("ShellPaths(cat readme.md) = %q, want only %q", got, side)
	}
}

// A wrapper runs the command after its own words (wrappers): its options and their values,
// NAME=value words and operands come first, nested wrappers too, and the command they run
// is read as any is, a shell's or an interpreter's script included (python3.12 -c, deno
// eval, php -r). env -C runs it in a folder of its own, env -S splits a string into its
// first words, and watch and flock -c run a shell script.
func TestShellPathsReadPastWrappers(t *testing.T) {
	t.Parallel()
	m := newWordsMachine(t)
	src := filepath.Join(m.side, "src")
	m.check(t, m.side, map[string][]string{
		"timeout -s KILL 60 sh -c 'cat ../acme-api/NOTES.md'":            {m.api},
		"sudo -u root timeout 5 env A=1 bash -c 'cd ../acme-api && ls'":  {m.api},
		"nohup nice -n 5 ../acme-api/NOTES.md":                           {m.api},
		"env -C ../acme-api cat NOTES.md; ls src":                        {m.api, src},
		"env -C../acme-api make":                                         {m.api},
		"env D=../acme-api bash -c 'ls $D'":                              {m.api},
		"env -S 'cat ../acme-api/NOTES.md'":                              {m.api},
		"watch -n 1 'cat ../acme-api/NOTES.md'":                          {m.api},
		"flock lockfile -c 'cat ../acme-api/NOTES.md'":                   {m.api},
		"ls | xargs -I {} sh -c 'cat ../acme-api/NOTES.md'":              {m.api},
		"uv run --with x python3.12 -c \"open('../acme-api/NOTES.md')\"": {m.api},
		"npm exec -- bash -c 'cd ../acme-api'":                           {m.api},
		"bundle exec ruby -e \"File.read('../acme-api/NOTES.md')\"":      {m.api},
		"deno eval \"Deno.readTextFileSync('../acme-api/NOTES.md')\"":    {m.api},
		"php -r \"file_get_contents('../acme-api/NOTES.md');\"":          {m.api},
		"perl -E 'say 1' ../acme-api/NOTES.md":                           {m.api},
		"cat <<'EOF' | sudo bash\ncd ../acme-api\nEOF":                   {m.api},
		"builtin cd ../acme-api && ls":                                   {m.api},
		"git -c core.pager=cat -C ../acme-api log":                       {m.api},
		"timeout 60 make test":                                           nil,
		`git commit -m "fix ../foo"`:                                     {m.side},
		`git commit -m "fix ../acme-api/NOTES.md"`:                       {m.side, m.api},
		"cd .. && cd side && cd .. && cat acme-api/NOTES.md":             {m.parent, m.side, m.api},
	})
	for _, command := range []string{`git -c k=v -C "$(cat p)" log`, "env -C`cat p` make", "env -S \"$CMD\""} {
		if got := m.paths(command, m.side); !slices.Contains(got, UnknownReach) {
			t.Errorf("ShellPaths(%q) = %q, without UnknownReach", command, got)
		}
	}
}

// A command that does not parse runs up to its error, as bash and zsh do: the statements
// before it are read as any are, a cd's move included, and the rest as its words from
// where they left the command. A cd, pushd or popd, process substitution, or unparsed
// shell metacharacter word in the rest goes somewhere not known.
func TestShellPathsReadACommandThatDoesNotParseUpToItsError(t *testing.T) {
	t.Parallel()
	m := newWordsMachine(t)
	src := filepath.Join(m.side, "src")
	m.check(t, m.side, map[string][]string{
		"ls src\ncat ../acme-api/NOTES.md\necho \"oops":       {src, m.api},
		"cd ..\nfor f (*.go) print $f\ncat acme-api/NOTES.md": {m.parent, m.api},
		"ls src\npushd ..\necho 'x":                           {src, m.parent},
		"cd .. && cat acme-api/NOTES.md; echo $((":            {m.parent, m.api},
	})
	for _, command := range []string{
		"cd ../acme-api && echo \"oops",
		"echo 'x\npopd",
		"ls (\npushd ../acme-api",
		"echo 'x\ncat <(cat ../acme-api/NOTES.md)",
	} {
		if got := m.paths(command, m.side); !slices.Contains(got, UnknownReach) {
			t.Errorf("ShellPaths(%q) = %q, without UnknownReach", command, got)
		}
	}
}

// A recursive reader (rg, find, grep -r, ls -R, cp -r, tar c, git grep --no-index...) of a
// folder outside any checkout reaches each checkout below it, at most maxBelowDepth levels
// down, from the folders it names or where the command is when it names none there. Past
// that depth, or the lookup bound, the reach is not known. A reader of a folder in a
// checkout reaches that checkout alone, and one of a file, or that is not recursive, only
// what it names. find -exec / fd -x ignore shallow depth limits.
func TestShellPathsReachTheCheckoutsBelowARecursiveRead(t *testing.T) {
	t.Parallel()
	m := newWordsMachine(t)
	deep, elsewhere := filepath.Join(m.parent, "work", "x", "y", "deep"), t.TempDir()
	for _, dir := range []string{filepath.Join(m.side, ".git"), filepath.Join(m.api, ".git"), filepath.Join(deep, ".git"), filepath.Join(elsewhere, "far", "a", "b", "c", "d", "e")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	below := []string{m.api, m.side, deep}
	m.check(t, m.side, map[string][]string{
		"rg -n TODO ..":                       append([]string{m.parent}, below...),
		"ls ..; rg -n TODO ..":                append([]string{m.parent}, below...),
		"cd .. && rg TODO":                    append([]string{m.parent}, below...),
		"grep -rn secret ~/../":               append([]string{m.parent}, below...),
		`find .. -exec cat {} \;`:             append([]string{m.parent}, below...),
		`find .. -maxdepth 1 -exec cat {} \;`: append([]string{m.parent}, below...),
		"fd --max-depth 1 -x cat {} ..":       append([]string{m.parent}, below...),
		"grep -d recurse TODO ..":             append([]string{m.parent}, below...),
		"tar czf out.tgz ..":                  append([]string{m.parent}, below...),
		"git grep --no-index TODO ..":         append([]string{m.parent}, below...),
		"rg TODO":                             nil,
		"grep -rn TODO ../acme-api/NOTES.md":  {m.api},
		"grep -n TODO ..":                     {m.parent},
		"ls ..":                               {m.parent},
		"git grep TODO ..":                    append([]string{m.parent}, below...),
		"find ../acme-api -name x":            {m.api},
		"cd ../work && ls -R":                 append([]string{filepath.Join(m.parent, "work")}, deep),
	})
	for i := range maxShellLookups {
		if err := os.MkdirAll(filepath.Join(elsewhere, "big", fmt.Sprint(i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	at := filepath.ToSlash(elsewhere)
	for _, command := range []string{"rg TODO " + at + "/far", "grep -r TODO " + at + "/big"} {
		if got := m.paths(command, m.side); !slices.Contains(got, UnknownReach) {
			t.Errorf("ShellPaths(%q) = %q, without UnknownReach", command, got)
		}
	}
}

// The output of a few command substitutions is known where the command is: mktemp's new
// folder in the temporary folder, Go's folders, and what dirname, basename, realpath and
// readlink -f make of their operand. mktemp given a folder is not known.
func TestShellPathsKnowSomeSubstitutions(t *testing.T) {
	t.Parallel()
	m := newWordsMachine(t)
	m.check(t, m.side, map[string][]string{
		`cat "$(dirname ../acme-api/NOTES.md)/NOTES.md"`: {m.api},
		"cat $(dirname $ACME/)/acme-api/NOTES.md":        {m.api},
		`cd "$(realpath ../acme-api)" && ls`:             {m.api},
		`cd "$(readlink -f ../acme-api)"`:                {m.api},
		"t=$(mktemp -d) && cp x $t/":                     {filepath.Join(m.parent, "tmp")},
		"ls $(go env GOPATH)/bin":                        {m.home},
		"ls $(go env GOMODCACHE)/x":                      {m.home},
	})
	m.check(t, m.parent, map[string][]string{"ls $(basename $ACME)": {m.api}})
	if got := m.paths("t=$(mktemp -p ../acme-api) && cp x $t/", m.side); !slices.Contains(got, UnknownReach) {
		t.Errorf("mktemp -p: %q, without UnknownReach", got)
	}
}

// The scan parses and expands at most maxShellBytes, all told, each word charged what it
// may expand to before it is, each word of a brace expansion too: a script run again and
// again, a here-document or a word that repeats a long variable, replacements that multiply
// one, or braces that do, reach somewhere not known at once rather than costing gigabytes.
// So does a word holding more than maxShellBraces braces.
func TestShellPathsChargeWhatTheyExpand(t *testing.T) {
	t.Parallel()
	m := newWordsMachine(t)
	m.env["S"] = strings.Repeat("a", 200<<10)
	for command, unknown := range map[string]bool{
		`echo "$S"`:                            false,
		strings.Repeat(`bash -c "$S"; `, 2000): true,
		"cat <<EOF\n" + strings.Repeat("$S", 2000) + "\nEOF":                            true,
		`echo "` + strings.Repeat("$S", 2000) + `"`:                                     true,
		"T=aaaaaaaaaa; U=${T//a/$T}; V=${U//a/$U}; W=${V//a/$V}; X=${W//a/$W}; echo $X": true,
		`bash -c {1..16000}"$S"`:                                                        true,
		`cat <<<{1..16000}"$S"`:                                                         true,
		"ls {a,b}/{c,d} f{,.bak} {1..3}":                                                false,
		"echo " + strings.Repeat("{", maxShellBraces):                                   false,
		"echo " + strings.Repeat("{", maxShellBraces+1):                                 true,
		"echo " + strings.Repeat("{", 4000):                                             true,
		"echo " + strings.Repeat("{a,b}", 1024):                                         true,
	} {
		start := time.Now()
		if got := m.paths(command, m.side); slices.Contains(got, UnknownReach) != unknown {
			t.Errorf("ShellPaths(%.40q…) = %.80q, want UnknownReach %v", command, got, unknown)
		}
		if took := time.Since(start); took > time.Second {
			t.Errorf("ShellPaths(%.40q…) took %v", command, took)
		}
	}
}

// A script past the parser's bounds is not parsed (parsable): nested too deeply, an
// expression too long, too many compound commands, pipes or lists, or too many words or
// expansions. Quotes, comments and here-documents are lexed as the shell does, so a body
// or a string that holds brackets or keywords counts for nothing, and one that closes a
// context the parser keeps open hides nothing.
func TestAScriptPastTheParsersBoundsIsNotParsed(t *testing.T) {
	t.Parallel()
	prose := strings.Repeat("if (a) for [b] while {c} case $d and don't (\n", 2000)
	for src, want := range map[string]bool{
		"cat > f <<'EOF'\n" + prose + "EOF\nls":                   true,
		"cat > f <<EOF\n" + prose + "EOF\nls":                     true,
		`git commit -m "` + strings.Repeat("if (a) ", 5000) + `"`: true,
		"echo $((1+2)) [[ -f x ]] ${a:-b} `ls` $(ls)":             true,
		strings.Repeat("(", maxShellNesting+1):                    false,
		strings.Repeat("{ ", maxShellNesting+1):                   false,
		strings.Repeat("$(", maxShellNesting+1):                   false,
		strings.Repeat("if a; then ", maxShellNesting+1):          false,
		strings.Repeat("a | ", maxShellNesting+1):                 false,
		"echo $((" + strings.Repeat("!", maxShellExpr) + "1))":    false,
		"echo ${a:" + strings.Repeat("!", maxShellExpr) + "1}":    false,
		"a[" + strings.Repeat("!", maxShellExpr) + "1]=x":         false,
		"let " + strings.Repeat("!", maxShellExpr+1):              false,
		"echo" + strings.Repeat(" w", maxShellWords+1):            false,
		"echo " + strings.Repeat("a$b", maxShellWords+1):          false,
		// What a shell holds open, the lexing does too.
		strings.Repeat(`$(echo ")" `, maxShellNesting+1):                 false,
		strings.Repeat("$(case a in a) ", maxShellNesting+1):             false,
		"${#x} # don't\n" + strings.Repeat("(", maxShellNesting+1):       false,
		"cat <<'EOF'\nx\nEOF\n" + strings.Repeat("(", maxShellNesting+1): false,
		strings.Repeat("$(cat <<EOF\n", maxShellNesting+1):               false,
	} {
		if got := parsable(src); got != want {
			t.Errorf("parsable(%.50q…) = %v, want %v", src, got, want)
		}
	}
	m := newWordsMachine(t)
	if got := m.paths(strings.Repeat("(", 1<<19), m.side); !slices.Equal(got, []string{UnknownReach}) {
		t.Errorf("half a megabyte of subshells: %q, want UnknownReach", got)
	}
}

// deepTree is a tree outside any checkout five levels deep, each folder holding five, deeper
// than a walk below a folder goes and wider than its lookups reach.
func deepTree(t testing.TB) string {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	level := []string{root}
	for range 5 {
		var next []string
		for _, dir := range level {
			for i := range 5 {
				next = append(next, filepath.Join(dir, fmt.Sprint("d", i)))
			}
		}
		for _, dir := range next {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		level = next
	}
	return root
}

// A scan ends at a hook's deadline (shellTime), its reach not known: a program reading below
// a deep tree outside any checkout ends within about shellTime, and a scan whose deadline has
// passed looks at nothing.
func TestShellPathsEndAtTheirDeadline(t *testing.T) {
	t.Parallel()
	tree := deepTree(t)
	command := "rg TODO " + filepath.ToSlash(tree)
	start := time.Now()
	got := shellPathsBy(command, tree, os.LookupEnv, start.Add(shellTime))
	if took := time.Since(start); !slices.ContainsFunc(got, isUnknownReach) || took > 5*shellTime {
		t.Errorf("ShellPaths(%q) = %q in %v, want UnknownReach within about %v", command, got, took, shellTime)
	}
	if sc := scanShell(command, tree, os.LookupEnv, work{deadline: time.Now().Add(-time.Second)}); !sc.unknown || sc.lookups != 0 {
		t.Errorf("past its deadline: unknown %v after %d lookups", sc.unknown, sc.lookups)
	}
}

// BenchmarkShellPathsBelowADeepTree times a program reading below deepTree, which ends at the
// scan's bounds.
func BenchmarkShellPathsBelowADeepTree(b *testing.B) {
	tree := deepTree(b)
	command := "rg TODO " + filepath.ToSlash(tree)
	for b.Loop() {
		if got := shellPaths(command, tree, os.LookupEnv); !slices.ContainsFunc(got, isUnknownReach) {
			b.Fatalf("ShellPaths(%q) = %q, without UnknownReach", command, got)
		}
	}
}
