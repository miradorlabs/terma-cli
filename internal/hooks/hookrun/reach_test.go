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
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/runs"
)

// A shell command's places are the folders of its absolute, ~/ and slashed words and every
// cd's target, split at whitespace and the shell's operators outside quotes, resolved
// against the cwd, where something is there: a folder is its own place, a file its
// folder's. A word with nothing there may be a file the command creates, so it reaches the
// nearest folder that is there: another checkout's for a new file there, the cwd's own for
// a word that only looks like a path. A cd to nowhere goes nowhere.
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
		"echo x >> '" + notes + "'":          {api},
		`cd "../acme api"&&ls`:               {api},
		`cd ../acme\ api || ls src/`:         {api, filepath.Join(cwd, "src")},
		"cat <'../acme api/NOTES.md'|wc -l":  {api},
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

		"echo $(cat '../acme api/NOTES.md')":          {api},
		"echo `cat ../acme\\ api/NOTES.md`":           {api},
		`echo "$(cat '../acme api/NOTES.md')"`:        {api},
		`echo "notes: $(cat "../acme api/NOTES.md")"`: {api},
		`x="$(cd '../acme api' && pwd)"`:              {api},
		"echo '$(cat ../acme\\ api/NOTES.md)'":        {cwd},
		"(cd '../acme api')":                          {api},
		"(cat '../acme api/NOTES.md')":                {api},
		"grep --file='../acme api/NOTES.md' TODO":     {api},
		"cc -I'../acme api' -o out.o x.c":             {api},
		"cc -o'../acme api/NOTES.md' x.c":             {api},
		"GIT_DIR='../acme api' git status":            {api},
		"pushd src && ls":                             {filepath.Join(cwd, "src")},
		"cd -P src":                                   {filepath.Join(cwd, "src")},
		"cd .. && cat 'acme api/NOTES.md'":            {parent, api},
		"(cd src) && cat '../acme api/NOTES.md'":      {filepath.Join(cwd, "src"), api},
		"cat src/../../'acme api'/NOTES.md src":       {api, filepath.Join(cwd, "src")},
	} {
		if got := ShellPaths(command, cwd); !sameReach(command, got, want) {
			t.Errorf("ShellPaths(%q) = %q, want %q", command, got, want)
		}
	}
}

// A URL names no path on this machine: its host and path are no //host/path, which Windows
// takes for another machine's (https://example.com/a is no \\example.com\a). A file: URL
// names its path. A path that starts with two separators is never looked up, as Windows sends
// it to the network, where a lookup may wait far past the hook's deadline: a share's
// (//host/share) is a path not resolved, so somewhere not known where one counts (an
// argument, a cd's target) and nothing where only one that is there does (prose); anything
// else so written (// TODO) names none. Where //x is /x (Unix), each would name Acme's
// checkout were it looked up. A path on the share the command runs in, which the hook reads
// anyway, is looked up as any.
func TestAURLOrAPathOnAnotherMachineIsNeverLookedUp(t *testing.T) {
	t.Parallel()
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	cwd, api := filepath.Join(parent, "side"), filepath.Join(parent, "acme-api")
	for _, dir := range []string{cwd, api} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(api, "NOTES.md"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Acme's checkout past its volume and leading slash: private/var/.../acme-api.
	rel := strings.TrimLeft(filepath.ToSlash(strings.TrimPrefix(api, filepath.VolumeName(api))), "/")
	for _, tc := range []struct {
		command string
		want    []string
		unknown bool
	}{
		{"curl https://" + rel + "/NOTES.md", nil, false},
		{"curl -fsSL https://" + rel + " > out", []string{cwd}, false},
		{"docker run -e URL=git+ssh://" + rel + " alpine", nil, false},
		{`node -e "fetch('https://` + rel + `')"`, nil, false},
		{"cat file:///" + strings.TrimPrefix(filepath.ToSlash(api), "/") + "/NOTES.md", []string{api}, false},
		{"cat //" + rel + "/NOTES.md", nil, true},
		{"cd //" + rel + " && ls", nil, true},
		{"ls //" + rel + "/*.md", nil, true},
		{"cat <<EOF\nsee //" + rel + "\nEOF", nil, false},
		{"rg -n '// TODO'", nil, false},
	} {
		got := ShellPaths(tc.command, cwd)
		if unknown := slices.ContainsFunc(got, isUnknownReach); unknown != tc.unknown || !slices.Equal(slices.DeleteFunc(got, isUnknownReach), tc.want) {
			t.Errorf("ShellPaths(%q) = %q, want %q, somewhere not known %v", tc.command, got, tc.want, tc.unknown)
		}
	}
	for _, tc := range []struct {
		path, cwd string
		remote    bool
	}{
		{"//host/share/acme-api", cwd, true},
		{"//host/share/acme-api", "//HOST/Share/side", false},
		{"//host/other/acme-api", "//host/share/side", true},
		{"// TODO", "// TODO", true},
		{"/" + rel, cwd, false},
		{"///" + rel, cwd, true},
	} {
		if got := remote(tc.path, tc.cwd); got != tc.remote {
			t.Errorf("remote(%q, %q) = %v, want %v", tc.path, tc.cwd, got, tc.remote)
		}
	}
}

// A command reaches its first maxShellPlaces places, each folder once however many of its
// words name it, and one reaching more counts as reaching an unknown organization: Acme's
// checkout among the first turns the run Mixed, and so does any command past the bound,
// though none of its places is another checkout. Words naming files of one folder are one
// place.
func TestAShellCommandPastThePlaceBoundTurnsTheRunMixed(t *testing.T) {
	t.Parallel()
	st := acmeAndPersonalRepos(t)
	side, api := hookruntest.RepoAt(t, "github.com/dawson/side"), hookruntest.RepoAt(t, "github.com/acme/api")
	for i := range maxShellPlaces + 5 {
		hookruntest.WriteFile(t, side, fmt.Sprintf("d%03d/a.go", i), "package d\n")
		hookruntest.WriteFile(t, side, fmt.Sprintf("d%03d/b.go", i), "package d\n")
	}
	for _, tc := range []struct {
		places int
		acmeAt int // Acme's position among the places, from 0; -1 for none
		mixed  bool
	}{{maxShellPlaces, 0, true}, {maxShellPlaces, -1, false}, {maxShellPlaces + 1, -1, true}, {maxShellPlaces + 5, maxShellPlaces, true}} {
		t.Run(fmt.Sprint(tc.places, tc.acmeAt), func(t *testing.T) {
			t.Parallel()
			m := newRoutedMachine(t, st)
			sid := fmt.Sprintf("long-command-%d-%d", tc.places, tc.acmeAt)
			r := startIn(t, m.hook(), sid, side)
			words := []string{"ls"}
			for i := range tc.places {
				if i == tc.acmeAt {
					// Quoted, as a shell needs a Windows path's backslashes.
					words = append(words, "'"+api+"'")
				} else {
					// Two files of one folder, and a new one there: one place.
					words = append(words, fmt.Sprintf("d%03d/a.go d%03d/b.go d%03d/new.go", i, i, i))
				}
			}
			paths := shellPathsBy(strings.Join(words, " "), side, os.LookupEnv, time.Now().Add(time.Minute))
			if past := slices.ContainsFunc(paths, isUnknownReach); past != (tc.places > maxShellPlaces) {
				t.Fatalf("ShellPaths named UnknownReach %v for %d places", past, tc.places)
			}
			env := m.hook()
			env.Cwd = side
			env.Classify(context.Background(), r, sessionOf(sid), Alone(paths...), nil)
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

// On Windows a backslash is the path separator, never an escape: an agent's shell command
// (PowerShell or cmd) names C:\Users\... unquoted, and it is still a path.
func TestAnUnquotedWindowsPathIsAPath(t *testing.T) {
	t.Parallel()
	if filepath.Separator != '\\' {
		t.Skip("Windows paths")
	}
	dir := t.TempDir()
	notes := filepath.Join(dir, "NOTES.md")
	if err := os.WriteFile(notes, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"cat " + notes, "Get-Content " + notes + " | Select-Object -First 5", `type "` + notes + `"`} {
		if got := ShellPaths(command, dir); !slices.Contains(got, dir) {
			t.Errorf("ShellPaths(%q) = %q, want %q among them", command, got, dir)
		}
	}
}

// sameReach reports whether ShellPaths found want for command. On Windows a command
// holding a backslash is also read with backslashes as separators (shellReadings), so it may
// find more: each path that reading adds can only make a run Mixed, never miss one.
func sameReach(command string, got, want []string) bool {
	if len(shellReadings(command, windowsShells)) == 1 {
		return slices.Equal(got, want)
	}
	for _, p := range want {
		if !slices.Contains(got, p) {
			return false
		}
	}
	return true
}

// A POSIX reading takes a backslash for an escape; the Windows reading, as PowerShell and
// cmd do, for a separator, so a path written with backslashes reaches what it names there.
// On Windows ShellPaths takes both readings.
func TestABackslashSeparatesOnlyInTheWindowsReading(t *testing.T) {
	t.Parallel()
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	cwd, api := filepath.Join(parent, "side"), filepath.Join(parent, "acme api")
	for _, dir := range []string{cwd, api} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	none := func(string) (string, bool) { return "", false }
	// On Windows the path package itself separates at a backslash, so even the POSIX
	// reading of a quoted backslash path reaches what it names there.
	var posixQuoted []string
	if filepath.Separator == '\\' {
		posixQuoted = []string{api}
	}
	for _, c := range []struct {
		command string
		windows bool
		want    []string
	}{
		{`cat ../acme\ api/NOTES.md`, false, []string{api}},
		{`cat "..\acme api\NOTES.md"`, true, []string{api}},
		{`cat "..\acme api\NOTES.md"`, false, posixQuoted},
		{`type ..\side`, true, []string{cwd}},
	} {
		src := c.command
		if c.windows {
			src = windowsReading(src)
		}
		if got := scanShell(src, cwd, none, work{}).out; !slices.Equal(got, c.want) {
			t.Errorf("%q, Windows reading %v: %q, want %q", c.command, c.windows, got, c.want)
		}
	}
	// The Windows reading goes on from the work the first did: the scan's bounds are the
	// command's, however many readings it takes.
	src := windowsReading(`type ..\side`)
	if sc := scanShell(src, cwd, none, work{bytes: maxShellBytes - len(src)}); sc.unknown {
		t.Errorf("%q with the bytes it parses left: unknown", src)
	}
	if sc := scanShell(src, cwd, none, work{bytes: maxShellBytes - len(src) + 1}); !sc.unknown {
		t.Errorf("%q past the bytes left: %q, without UnknownReach", src, sc.out)
	}
}

// Classify reads below each folder it is given, as a search does (the folder-below rule): a
// folder outside any checkout reaches each checkout below it, personal's and Acme's beside
// each other turning the run Mixed, and so does the folder a glob names before its first
// wildcard (../**/*.go). Past the walk's bounds the reach is not known, the ledger naming the
// folder. A folder alone, a listing's or a shell command's place, reaches itself alone, and a
// search of a checkout that checkout alone.
func TestClassifyReadsBelowAFolderOutsideAnyCheckout(t *testing.T) {
	t.Parallel()
	c := newCorpus(t)
	deep, _ := filepath.EvalSymlinks(t.TempDir())
	hookruntest.WriteFile(t, deep, "a/b/c/d/e/f/x.go", "package f\n")
	crossed := runs.Why{Reason: runs.Crossed, Where: "github.com/acme/acme-api"}
	for name, tc := range map[string]struct {
		paths []string
		want  runs.Why
	}{
		"the folder above":           {[]string{c.home}, crossed},
		"the folder above, relative": {[]string{".."}, crossed},
		"a glob above its checkout":  {Search(c.side, "../**/*.go"), crossed},
		"Acme's checkout":            {Search(c.api), crossed},
		"a tree past the walk":       {[]string{deep}, runs.Why{Reason: runs.UnknownReach, Where: firstWords(deep)}},
		"a share's folder":           {[]string{"//host/share/acme-api"}, runs.Why{Reason: runs.UnknownReach, Where: firstWords("//host/share/acme-api")}},
		"a share's folder, alone":    {Alone("//host/share/acme-api"), runs.Why{Reason: runs.UnknownReach, Where: firstWords("//host/share/acme-api")}},
		"the folder above, alone":    {Alone(c.home), runs.Why{}},
		"its own checkout":           {Search(c.side, "**/*.go"), runs.Why{}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newRoutedMachine(t, acmeAndPersonalRepos(t))
			sid := "below-" + strings.Map(func(r rune) rune {
				if r < 'a' || r > 'z' {
					return '-'
				}
				return r
			}, strings.ToLower(name))
			r := startIn(t, m.hook(), sid, c.side)
			env := m.hook()
			env.Cwd = c.side
			env.Classify(context.Background(), r, sessionOf(sid), tc.paths, nil)
			if why := m.why(sid); why != tc.want {
				t.Errorf("the ledger says the run is Mixed for %+v, want %+v", why, tc.want)
			}
		})
	}
}
