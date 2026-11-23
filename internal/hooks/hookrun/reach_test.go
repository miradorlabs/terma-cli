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
// words name it, and one reaching more appends UnknownReach: Acme's checkout among the first
// is recorded, and any command past the bound reports UnknownReach even when every folder is
// inside the checkout. Words naming files of one folder are one place.
func TestAShellCommandPastThePlaceBoundIsUnknown(t *testing.T) {
	t.Parallel()
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	side := filepath.Join(parent, "side")
	api := filepath.Join(parent, "acme-api")
	if err := os.MkdirAll(api, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range maxShellPlaces + 5 {
		dir := filepath.Join(side, fmt.Sprintf("d%03d", i))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"a.go", "b.go"} {
			if err := os.WriteFile(filepath.Join(dir, f), []byte("package d\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tc := range []struct {
		places      int
		acmeAt      int // Acme's position among the places, from 0; -1 for none
		wantUnknown bool
		wantAcme    bool
	}{
		{maxShellPlaces, 0, false, true},
		{maxShellPlaces, -1, false, false},
		{maxShellPlaces + 1, -1, true, false},
		{maxShellPlaces + 5, maxShellPlaces, true, false},
	} {
		t.Run(fmt.Sprint(tc.places, tc.acmeAt), func(t *testing.T) {
			t.Parallel()
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
			if got := slices.ContainsFunc(paths, isUnknownReach); got != tc.wantUnknown {
				t.Fatalf("ShellPaths UnknownReach = %v for %d places, want %v", got, tc.places, tc.wantUnknown)
			}
			if got := slices.Contains(paths, api); got != tc.wantAcme {
				t.Fatalf("ShellPaths contains api = %v, want %v", got, tc.wantAcme)
			}
		})
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
	// So are the places it reaches: p\001 is p001 read as POSIX, and q\001 q/001 read as on
	// Windows, each reading's within maxShellPlaces and both past it.
	words := []string{"ls"}
	for i := range maxShellPlaces/2 + 10 {
		for _, dir := range []string{fmt.Sprintf("p%03d", i), fmt.Sprintf("q/%03d", i)} {
			if err := os.MkdirAll(filepath.Join(cwd, dir), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		words = append(words, fmt.Sprintf(`p\%03d q\%03d`, i, i))
	}
	command := strings.Join(words, " ")
	for _, windows := range []bool{false, true} {
		got := readShell(command, shellReadings(command, windows), cwd, none, time.Now().Add(time.Minute))
		if unknown := slices.ContainsFunc(got, isUnknownReach); unknown != windows || len(got) > maxShellPlaces+1 {
			t.Errorf("%d places, read as on Windows %v: somewhere not known %v", len(got), windows, unknown)
		}
	}
}

// Alone tags each path so a caller can distinguish a path alone from a folder search; Search
// and searchEnv resolve a folder and glob patterns (including braces and ~/) to their base
// folders before the first wildcard; searchedEnv filters out paths that name nothing on disk.
func TestAloneAndSearchResolveFoldersAndGlobs(t *testing.T) {
	t.Parallel()
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	side := filepath.Join(parent, "side")
	api := filepath.Join(parent, "acme-api")
	home := filepath.Join(parent, "home")
	for _, dir := range []string{filepath.Join(side, "src"), api, filepath.Join(home, "proj")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	lookup := func(k string) (string, bool) {
		if k == "HOME" {
			return home, true
		}
		return "", false
	}

	// Alone marks every non-empty path idempotently and preserves UnknownReach untouched.
	alone := Alone(side, "", UnknownReach, aloneMark+api)
	wantAlone := []string{aloneMark + side, "", UnknownReach, aloneMark + api}
	if !slices.Equal(alone, wantAlone) {
		t.Errorf("Alone = %q, want %q", alone, wantAlone)
	}
	if got := Alone(); len(got) != 0 {
		t.Errorf("Alone() = %q, want empty", got)
	}

	// searchEnv resolves folders, globs, braces, and tilde prefixes.
	for _, tc := range []struct {
		name        string
		dir         string
		patterns    []string
		want        []string
		wantUnknown bool
	}{
		{"folder alone", side, nil, []string{side}, false},
		{"glob below folder", side, []string{"**/*.go"}, []string{side}, false},
		{"glob above folder", side, []string{"../**/*.go"}, []string{side, parent}, false},
		{"brace alternation", side, []string{"../{acme-api,side}/**/*.go"}, []string{side, api, side}, false},
		{"nested braces fail closed", side, []string{"../{a,{acme-api,side}}/*.go"}, []string{side}, true},
		{"unclosed non-slashed brace literal", side, []string{"src/file{a.go"}, []string{side, filepath.Join(side, "src")}, false},
		{"escaped wildcard literal", side, []string{`src/foo\[x\]/*.go`}, []string{side, filepath.Join(side, "src", "foo[x]")}, false},
		{"tilde glob", side, []string{"~/proj/**/*.go"}, []string{side, filepath.Join(home, "proj")}, false},
		{"tilde bare", side, []string{"~"}, []string{side, home}, false},
		{"tilde user fails closed", side, []string{"~other/proj/*.go"}, []string{side}, true},
		{"remote pattern", side, []string{"//host/share/**/*.go"}, []string{side, "//host/share"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := searchEnv(tc.dir, lookup, tc.patterns...)
			gotUnknown := slices.ContainsFunc(got, isUnknownReach)
			clean := slices.DeleteFunc(slices.Clone(got), isUnknownReach)
			if gotUnknown != tc.wantUnknown || !slices.Equal(clean, tc.want) {
				t.Errorf("searchEnv(%q, %q) = %q (unknown=%v), want %q (unknown=%v)", tc.dir, tc.patterns, clean, gotUnknown, tc.want, tc.wantUnknown)
			}
		})
	}

	// Unset HOME fails closed on ~/ glob patterns.
	noHome := func(string) (string, bool) { return "", false }
	if got := searchEnv(side, noHome, "~/proj/*.go"); !slices.ContainsFunc(got, isUnknownReach) {
		t.Errorf("searchEnv with unset HOME = %q, want UnknownReach", got)
	}

	// Public Search returns folders using os.LookupEnv.
	if got := Search(side, "../**/*.go"); !slices.Equal(got, []string{side, parent}) {
		t.Errorf("Search = %q, want %q", got, []string{side, parent})
	}

	// Brace expansion over the cap or too many { fails closed with UnknownReach.
	for _, pat := range []string{strings.Repeat("{a,b}", 8) + "/*.go", strings.Repeat("{a}", 40) + "/*.go"} {
		if got := searchEnv(side, lookup, pat); !slices.ContainsFunc(got, isUnknownReach) {
			t.Errorf("searchEnv(%q) = %q, want UnknownReach", pat, got)
		}
	}

	// searchedEnv discovers checkouts below non-Alone folders, deduplicates, skips Alone, and maps remote paths to UnknownReach.
	for _, dir := range []string{filepath.Join(side, ".git"), filepath.Join(api, ".git")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	gotBelow := searchedEnv([]string{parent, ".."}, side, lookup)
	if !slices.Contains(gotBelow, api) || !slices.Contains(gotBelow, side) {
		t.Errorf("searchedEnv(parent) = %q, want both %q and %q", gotBelow, api, side)
	}
	gotAlone := searchedEnv(Alone(parent, filepath.Join(api, "NOTES.md")), side, lookup)
	if !slices.Equal(gotAlone, []string{parent, filepath.Join(api, "NOTES.md")}) {
		t.Errorf("searchedEnv(Alone) = %q, want direct paths only", gotAlone)
	}
	gotRemote := searchedEnv([]string{"//host/share/dir"}, side, lookup)
	if len(gotRemote) != 1 || !isUnknownReach(gotRemote[0]) {
		t.Errorf("searchedEnv(remote) = %q, want UnknownReach", gotRemote)
	}
	// Expired deadline marks UnknownReach while preserving later direct paths.
	gotExpired := searchedBy([]string{parent, api}, side, lookup, time.Now().Add(-time.Second))
	if !slices.Contains(gotExpired, api) || !slices.ContainsFunc(gotExpired, isUnknownReach) {
		t.Errorf("searchedBy(expired) = %q, want %q and UnknownReach", gotExpired, api)
	}
}
