package hookrun

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"mvdan.cc/sh/v3/syntax"
)

func init() {
	maxShellTime = time.Second
}

// firstWords masks secret option values, assignment values, short option values, and URL
// userinfo, and caps output at 4 words / 60 runes.
func TestFirstWordsMasksSecretsAndCapsLength(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"TOKEN=abc123 curl -H 'Authorization: Bearer xyz' https://user:pw@example.com/a": "TOKEN=… curl -H … …",
		"curl -u alice:secret https://example.com":                                       "curl -u … https://example.com",
		"curl -U alice:secret https://example.com":                                       "curl -U … https://example.com",
		"curl --proxy-user alice:secret https://example.com":                             "curl --proxy-user … https://example.com",
		"curl -sU alice:secret https://example.com":                                      "curl -s… … https://example.com",
		"redis-cli -a mypass ping":                                                       "redis-cli -a … ping",
		"ENV=x redis-cli -a mypass":                                                      "ENV=… redis-cli -a …",
		"sudo redis-cli -a mypass":                                                       "sudo redis-cli -a …",
		`echo don\'t --password mypass`:                                                  `echo don\'t --password …`,
		"curl -d secretbody https://example.com":                                         "curl -d … https://example.com",
		"curl --oauth2-bearer tok https://example.com":                                   "curl --oauth2-bearer … https://example.com",
		"cli --api-key supersecret run":                                                  "cli --api-key … run",
		`cli "--password" hunter2 run`:                                                   `cli "--password" … run`,
		"curl https://u:p@ss@example.com/a":                                              "curl https://…@example.com/a",
		"curl https://alice:supersecret=foo@example.com":                                 "curl https://…@example.com",
		"curl https://alice:supersecret=foo@example.com?q=1":                             "curl https://…@example.com?q=…",
		"URL=https://alice:supersecret=foo@example.com echo hi":                          "URL=… echo hi",
		"cli --token=supersecret":                                                        "cli --token=…",
		"cli -pfoo run":                                                                  "cli -p… run",
		"echo " + strings.Repeat("a", 80):                                                "echo " + strings.Repeat("a", 55) + " …",
		"echo " + strings.Repeat("é", 70):                                                "echo " + strings.Repeat("é", 55) + " …",
		"echo " + strings.Repeat("é", 2100):                                              "echo " + strings.Repeat("é", 55) + " …",
		strings.Repeat("\x80", 4100):                                                     " …",
		"echo hi\x00there":                                                               "echo hi there",
		"curl -sH 'Authorization: Bearer xyz' https://example.com":                       "curl -s… … https://example.com",
		"redis-cli -ha mypass ping":                                                      "redis-cli -h… … ping",
		"one two three four five":                                                        "one two three four …",
	}
	for cmd, want := range cases {
		if got := firstWords(cmd); got != want {
			t.Errorf("firstWords(%q) = %q, want %q", cmd, got, want)
		}
	}
	if u := unknownReach("echo hi"); !isUnknownReach(u) || u != UnknownReach+"\x00echo hi" {
		t.Errorf("unknownReach = %q", u)
	}
}

// parsable enforces nesting, expression length, compound/pipe, and word/dollar bounds
// while ignoring brackets and keywords inside quotes, comments, and quoted heredocs.
func TestParsableBounds(t *testing.T) {
	t.Parallel()
	prose := strings.Repeat("if (a) for [b] while {c} case $d and don't (\n", 2000)
	for src, want := range map[string]bool{
		"cat > f <<'EOF'\n" + prose + "EOF\nls": true,
		"cat > f <<EOF\n" + prose + "EOF\nls":   true,
		"cat <<-EOF\n\t\tEOF\nls":               true,
		"cat <<EOF\n\tEOF\nit's\nEOF\necho 'a' " + strings.Repeat("(", maxShellNesting+1) + " 'b'": false,
		`cat <<"A\"B"` + "\nbody\nA\"B\n" + strings.Repeat("(", maxShellNesting+1):                 false,
		"cat <<'EOF'\nunterminated":                                false,
		"cat <<$'EOF'\nbody\nEOF":                                  false,
		`git commit -m "` + strings.Repeat("if (a) ", 5000) + `"`:  true,
		"echo $((1+2)) [[ -f x ]] ${a:-b} `ls` $(ls)":              true,
		strings.Repeat("(", maxShellNesting+1):                     false,
		strings.Repeat("{ ", maxShellNesting+1):                    false,
		strings.Repeat("$(", maxShellNesting+1):                    false,
		strings.Repeat("if a; then ", maxShellNesting+1):           false,
		strings.Repeat("a | ", maxShellNesting+1):                  false,
		"echo $((" + strings.Repeat("!", maxShellExpr) + "1))":     false,
		"echo ${a:" + strings.Repeat("!", maxShellExpr) + "1}":     false,
		"a[" + strings.Repeat("!", maxShellExpr) + "1]=x":          false,
		"let " + strings.Repeat("!", maxShellExpr+1):               false,
		"echo" + strings.Repeat(" w", maxShellWords+1):             false,
		"echo " + strings.Repeat("a$b", maxShellWords+1):           false,
		"`echo" + strings.Repeat(" w", maxShellWords+1) + "`":      false,
		"cat <<< \"x\"\n" + strings.Repeat("(", maxShellNesting+1): false,
		"let x=1<<2\n" + strings.Repeat("(", maxShellNesting+1):    false,
		strings.Repeat(`$(echo ")" `, maxShellNesting+1):           false,
		strings.Repeat("$(case a in a) ", maxShellNesting+1):       false,
		"${#x} # don't\n" + strings.Repeat("(", maxShellNesting+1): false,
	} {
		if got := parsable(src); got != want {
			t.Errorf("parsable(%.50q…) = %v, want %v", src, got, want)
		}
	}
}

// A cached folder candidate still replays its folder operands onto a later command's
// sc.reads (the root cause behind `ls .. && rg -n TODO ..` missing `..`).
func TestCandidateCacheHitReplaysFolderOperands(t *testing.T) {
	t.Parallel()
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	side := filepath.Join(parent, "side")
	if err := os.MkdirAll(side, 0o700); err != nil {
		t.Fatal(err)
	}
	sc := newScan(side, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})

	// First command (e.g. `ls ..`): cache miss populates sc.examined.
	sc.reads = &operands{}
	sc.candidate("..", plain)
	if !sc.reads.named || !slices.Contains(sc.reads.folders, parent) {
		t.Fatalf("first command reads = %+v, want parent %q", sc.reads, parent)
	}

	// Second command (e.g. `rg -n TODO ..`): cache hit MUST still record parent in sc.reads.
	sc.reads = &operands{}
	sc.candidate("..", plain)
	if !sc.reads.named || !slices.Contains(sc.reads.folders, parent) {
		t.Fatalf("second command reads (cache hit) = %+v, want parent %q", sc.reads, parent)
	}
}

// below does not stop at an enclosing checkout (~/.git dotfiles repo or a parent checkout
// enclosing cwd's own repo), so sibling checkouts beneath it are still discovered, while a
// normal checkout (cwd's own repo or a sibling repo) still stops the walk.
func TestBelowDoesNotStopAtDotfilesOrEnclosingCheckout(t *testing.T) {
	t.Parallel()
	// Case 1: ~/.git dotfiles repository enclosing ~/work/side and ~/work/acme-api.
	home, _ := filepath.EvalSymlinks(t.TempDir())
	for _, p := range []string{
		filepath.Join(home, ".git"),
		filepath.Join(home, "work", "side", ".git"),
		filepath.Join(home, "work", "acme-api", ".git"),
		filepath.Join(home, "scratch", "acme-api", ".git"),
	} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	env := func(k string) (string, bool) {
		if k == "HOME" {
			return home, true
		}
		return os.LookupEnv(k)
	}

	side := filepath.Join(home, "work", "side")
	api := filepath.Join(home, "work", "acme-api")
	// Walking ~/work from ~/work/side must find ~/work/acme-api despite ~/.git.
	sc := newScan(side, env, work{deadline: time.Now().Add(time.Minute)})
	sc.below(filepath.Join(home, "work"), maxBelowDepth+2)
	if !slices.Contains(sc.out, api) {
		t.Fatalf("below(~/work) with ~/.git = %q, want %q", sc.out, api)
	}
	// Walking cwd's own checkout (~/work/side) still stops at side.
	scOwn := newScan(side, env, work{deadline: time.Now().Add(time.Minute)})
	scOwn.below(side, maxBelowDepth+2)
	if len(scOwn.out) != 0 {
		t.Fatalf("below(own repo) = %q, want empty", scOwn.out)
	}
	// Walking ~/scratch (outside any project repo, under ~/.git) finds ~/scratch/acme-api.
	scratch := filepath.Join(home, "scratch")
	scScratch := newScan(scratch, env, work{deadline: time.Now().Add(time.Minute)})
	scScratch.below(scratch, maxBelowDepth+2)
	if want := filepath.Join(scratch, "acme-api"); !slices.Contains(scScratch.out, want) {
		t.Fatalf("below(~/scratch) with ~/.git = %q, want %q", scScratch.out, want)
	}

	// Case 2: Non-$HOME enclosing checkout /ws/.git above /ws/side/.git and /ws/acme-api/.git.
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	for _, p := range []string{
		filepath.Join(ws, ".git"),
		filepath.Join(ws, "side", ".git"),
		filepath.Join(ws, "acme-api", ".git"),
	} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	scEnc := newScan(filepath.Join(ws, "side"), env, work{deadline: time.Now().Add(time.Minute)})
	scEnc.below(ws, maxBelowDepth+2)
	if want := filepath.Join(ws, "acme-api"); !slices.Contains(scEnc.out, want) {
		t.Fatalf("below(enclosing ws) = %q, want %q", scEnc.out, want)
	}
}

// URL parsing and UNC share safety: non-file URLs produce no path, file: URLs extract the
// local or share path, and remote() guards against off-share UNC paths.
func TestURLPathAndRemote(t *testing.T) {
	t.Parallel()
	wantDrive := map[bool]string{true: "C:/work/api", false: "/C:/work/api"}[filepath.Separator == '\\']
	for _, tc := range []struct {
		in    string
		path  string
		isURL bool
		bad   bool
	}{
		{"https://example.com/a/b", "", true, false},
		{"git+ssh://github.com/acme/api", "", true, false},
		{"file:///tmp/acme-api/NOTES.md", "/tmp/acme-api/NOTES.md", true, false},
		{"file://localhost/tmp/acme-api/NOTES.md", "/tmp/acme-api/NOTES.md", true, false},
		{"file://LOCALHOST/C:/work/api", wantDrive, true, false},
		{"file:///ws/priva%74e/README", "/ws/private/README", true, false},
		{"file:///ws/%2e%2e/acme-api/README", "/ws/../acme-api/README", true, false},
		{"file:///C:/work/api", wantDrive, true, false},
		{"file://server/share/api", "//server/share/api", true, false},
		{"file:///ws/%ZZ/README", "", true, true},
		{"file:///ws/%00/README", "", true, true},
		{"../acme-api/NOTES.md", "", false, false},
		{"C:/work/api", "", false, false},
	} {
		p, ok, bad := urlPath(tc.in)
		if p != tc.path || ok != tc.isURL || bad != tc.bad {
			t.Errorf("urlPath(%q) = (%q, %v, %v), want (%q, %v, %v)", tc.in, p, ok, bad, tc.path, tc.isURL, tc.bad)
		}
	}
	for _, tc := range []struct {
		p, cwd string
		want   bool
	}{
		{"//host/share/api", "/tmp/side", true},
		{"//host/share/api", "//HOST/Share/side", false},
		{"//host/other/api", "//host/share/side", true},
		{"// TODO", "// TODO", true},
		{"/tmp/api", "/tmp/side", false},
		{"///tmp/api", "/tmp/side", true},
	} {
		if got := remote(tc.p, tc.cwd); got != tc.want {
			t.Errorf("remote(%q, %q) = %v, want %v", tc.p, tc.cwd, got, tc.want)
		}
	}
}

// Known command substitutions resolve deterministically against the scan's directory and env.
func TestShellSubstKnownOutputs(t *testing.T) {
	t.Parallel()
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	side := filepath.Join(parent, "side")
	if err := os.MkdirAll(filepath.Join(side, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := func(k string) (string, bool) {
		switch k {
		case "HOME", "USERPROFILE":
			return parent, true
		case "GOPATH":
			return filepath.Join(parent, "go"), true
		case "GOENV", "GOMODCACHE", "XDG_CONFIG_HOME", "AppData":
			return "", false
		}
		return os.LookupEnv(k)
	}
	sc := newScan(side, env, work{deadline: time.Now().Add(time.Minute)})
	evalSubst := func(s *scan, src string) (string, error) {
		f, err := syntax.NewParser().Parse(strings.NewReader("echo $("+src+")"), "")
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		ce := f.Stmts[0].Cmd.(*syntax.CallExpr)
		cs := ce.Args[1].Parts[0].(*syntax.CmdSubst)
		var buf bytes.Buffer
		err = s.substitute(&buf, cs)
		return buf.String(), err
	}
	for src, want := range map[string]string{
		"pwd":                           side,
		"git rev-parse --show-toplevel": side,
		"go env GOPATH":                 filepath.Join(parent, "go"),
		"go env GOMODCACHE":             filepath.Join(parent, "go", "pkg", "mod"),
		"dirname /a/b/c":                filepath.FromSlash("/a/b"),
		"dirname link/../file":          filepath.FromSlash("link/.."),
		"dirname /foo":                  string(filepath.Separator),
		"dirname foo":                   ".",
		"basename /a/b/c":               "c",
		`basename ""`:                   "",
		"realpath .":                    side,
		"readlink -f .":                 side,
		"pwd -P":                        side,
		"dirname $(pwd)":                parent,
	} {
		got, err := evalSubst(sc, src)
		if err != nil || got != want {
			t.Errorf("substitute(%q) = (%q, %v), want (%q, nil)", src, got, err, want)
		}
	}
	if got, err := evalSubst(sc, "mktemp -d"); err != nil || !strings.HasPrefix(got, os.TempDir()) {
		t.Errorf("substitute(mktemp -d) = (%q, %v)", got, err)
	}
	if got, err := evalSubst(sc, "mktemp -dt foo.XXX"); err != nil || !strings.HasPrefix(got, os.TempDir()) {
		t.Errorf("substitute(mktemp -dt) = (%q, %v)", got, err)
	}
	scUnresolvedTmp := newScan(side, env, work{deadline: time.Now().Add(time.Minute)})
	scUnresolvedTmp.vars["TMPDIR"] = nil
	if got, err := evalSubst(scUnresolvedTmp, "mktemp -d"); err == nil {
		t.Errorf("substitute(mktemp -d) with unresolved TMPDIR = %q, want error", got)
	}
	for _, opaque := range []string{
		"cat somefile", "mktemp -p /tmp", "mktemp -dp/tmp", "mktemp --tmpdir=/tmp",
		"mktemp sub/foo.XXX", "realpath missing/parent", "readlink -f missing/parent",
	} {
		if _, err := evalSubst(sc, opaque); err == nil {
			t.Errorf("substitute(%q) succeeded, want error", opaque)
		}
	}

	// Persisted `go env -w` file makes unset GOPATH/GOMODCACHE fail closed; GOENV=off or explicit env still resolves.
	goEnvFile := filepath.Join(parent, "goenv")
	if err := os.WriteFile(goEnvFile, []byte("GOPATH=/other\nGOMODCACHE=/other/mod\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envWithGoEnv := func(extra map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) {
			if v, ok := extra[k]; ok {
				return v, true
			}
			if k == "HOME" || k == "USERPROFILE" {
				return parent, true
			}
			return "", false
		}
	}
	scGoEnv := newScan(side, envWithGoEnv(map[string]string{"GOENV": goEnvFile}), work{deadline: time.Now().Add(time.Minute)})
	for _, cmd := range []string{"go env GOPATH", "go env GOMODCACHE"} {
		if _, err := evalSubst(scGoEnv, cmd); err == nil {
			t.Errorf("substitute(%q) with existing GOENV succeeded, want error", cmd)
		}
	}
	scGoEnvExplicit := newScan(side, envWithGoEnv(map[string]string{"GOENV": goEnvFile, "GOPATH": filepath.Join(parent, "custom")}), work{deadline: time.Now().Add(time.Minute)})
	if got, err := evalSubst(scGoEnvExplicit, "go env GOPATH"); err != nil || got != filepath.Join(parent, "custom") {
		t.Errorf("explicit GOPATH with GOENV = (%q, %v), want custom", got, err)
	}
	if _, err := evalSubst(scGoEnvExplicit, "go env GOMODCACHE"); err == nil {
		t.Error("unset GOMODCACHE with GOENV succeeded, want error")
	}
	scGoEnvOff := newScan(side, envWithGoEnv(map[string]string{"GOENV": "off"}), work{deadline: time.Now().Add(time.Minute)})
	if got, err := evalSubst(scGoEnvOff, "go env GOPATH"); err != nil || got != filepath.Join(parent, "go") {
		t.Errorf("GOENV=off go env GOPATH = (%q, %v), want default", got, err)
	}
	scUnresolvedGoEnv := newScan(side, envWithGoEnv(nil), work{deadline: time.Now().Add(time.Minute)})
	scUnresolvedGoEnv.vars["GOENV"] = nil
	if _, err := evalSubst(scUnresolvedGoEnv, "go env GOPATH"); err == nil {
		t.Error("unresolved GOENV var succeeded, want error")
	}
}

// reachNew skips oversized base64-like tokens and charges each directory walk step so a
// hook never spends seconds walking a fake path.
func TestReachNewIsBounded(t *testing.T) {
	t.Parallel()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	huge := strings.Repeat("abc/", 5000) + "end"
	start := time.Now()
	sc := newScan(dir, os.LookupEnv, work{deadline: start.Add(shellTime)})
	sc.candidate(huge, plain)
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("oversized slashed word took %v", took)
	}
	// Deep nonexistent path stops immediately at the first missing component, while an existing
	// deep directory tree charges the stat budget per traversed component.
	deep := strings.Repeat("a/", 50) + "leaf"
	sc2 := newScan(dir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	sc2.candidate(deep, plain)
	if sc2.stats == 0 || sc2.stats > 30 {
		t.Fatalf("deep nonexistent path spent %d stats, want fast prefix stop in (0, 30]", sc2.stats)
	}
	deepDir := filepath.Join(dir, strings.Repeat("d/", 40))
	if err := os.MkdirAll(deepDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sc3 := newScan(dir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	sc3.candidate(strings.Repeat("d/", 40)+"leaf", plain)
	if sc3.stats < 40 {
		t.Fatalf("deep existing path spent only %d stats, want >= 40", sc3.stats)
	}
}

// Scan bounds: maxShellPlaces, maxShellBraces, charge, size, away, symlink loops, and holdsNone.
func TestScanBoundsAndSafety(t *testing.T) {
	t.Parallel()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	sc := newScan(dir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	for i := range maxShellPlaces {
		sc.place(filepath.Join(dir, string(rune('a'+i%26)), strings.Repeat("x", i/26+1)))
	}
	if sc.unknown {
		t.Fatal("unknown at exactly maxShellPlaces")
	}
	sc.place(filepath.Join(dir, "one-more"))
	if !sc.unknown {
		t.Fatal("not unknown past maxShellPlaces")
	}

	// charge enforces maxShellBytes and maxShellBraces rejects excessive brace words.
	scBytes := newScan(dir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	if scBytes.charge(maxShellBytes+1) || !scBytes.unknown {
		t.Fatal("charge did not fail closed past maxShellBytes")
	}
	braceCmd := "echo " + strings.Repeat("{a,b}", maxShellBraces+1)
	f, err := syntax.NewParser().Parse(strings.NewReader(braceCmd), "")
	if err != nil {
		t.Fatal(err)
	}
	scBrace := newScan(dir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	if _, err := scBrace.fields(f.Stmts[0].Cmd.(*syntax.CallExpr).Args[1]); err == nil || !scBrace.unknown {
		t.Fatal("fields past maxShellBraces did not fail closed")
	}
	// paramSize bounds quadratic replacements and anchored empty-var replacements.
	big := strings.Repeat("a", 2048)
	scSize := newScan(dir, func(k string) (string, bool) {
		if k == "BIG" {
			return big, true
		}
		return "", false
	}, work{deadline: time.Now().Add(time.Minute)})
	fSize, err := syntax.NewParser().Parse(strings.NewReader(`echo "${BIG//a/$BIG}" "${EMPTY/#/$BIG}"`), "")
	if err != nil {
		t.Fatal(err)
	}
	args := fSize.Stmts[0].Cmd.(*syntax.CallExpr).Args
	if got := scSize.size(args[1]); got != maxShellBytes+1 {
		t.Fatalf("size(quadratic repl) = %d, want %d", got, maxShellBytes+1)
	}
	if got := scSize.size(args[2]); got < len(big) {
		t.Fatalf("size(empty anchored repl) = %d, want >= %d", got, len(big))
	}

	// away marks unknown on remote shares in roles that must resolve, and 2-hop remote symlinks do too.
	scAway := newScan(dir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	scAway.candidate("//host/share/api", plain)
	if !scAway.unknown {
		t.Fatal("remote share in plain role did not mark unknown")
	}
	remoteLink := filepath.Join(dir, "unc-link")
	hopLink := filepath.Join(dir, "hop-link")
	if err := os.Symlink("//host/share/api", remoteLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(remoteLink, hopLink); err != nil {
		t.Fatal(err)
	}
	scLink := newScan(dir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	scLink.candidate(hopLink, plain)
	if !scLink.unknown {
		t.Fatal("2-hop symlink to remote share did not mark unknown")
	}

	// Symlink loop under below terminates within the lookup budget without hanging.
	loopDir := filepath.Join(dir, "loop")
	if err := os.MkdirAll(loopDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(loopDir, filepath.Join(loopDir, "self")); err != nil {
		t.Fatal(err)
	}
	scLoop := newScan(dir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	scLoop.below(loopDir, maxBelowDepth+2)
	if !scLoop.unknown {
		t.Fatal("symlink loop deeper than maxBelowDepth did not mark unknown")
	}

	// holdsNone skips node_modules, ~/Library, and GOMODCACHE.
	scNone := newScan(dir, func(k string) (string, bool) {
		switch k {
		case "HOME":
			return dir, true
		case "GOMODCACHE":
			return filepath.Join(dir, "modcache"), true
		}
		return "", false
	}, work{deadline: time.Now().Add(time.Minute)})
	if !scNone.holdsNone(filepath.Join(dir, "node_modules")) ||
		!scNone.holdsNone(filepath.Join(dir, "Library", "Caches")) ||
		!scNone.holdsNone(filepath.Join(dir, "modcache", "pkg")) {
		t.Fatal("holdsNone missed node_modules, ~/Library, or GOMODCACHE")
	}

	// Remote parent symlink on a nonexistent child (link/new) must fail closed in reachNew and real without statting.
	scNew := newScan(dir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	scNew.candidate(filepath.Join(remoteLink, "new"), creates)
	if !scNew.unknown {
		t.Fatal("reachNew through remote parent symlink did not mark unknown")
	}
	scReal := newScan(dir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	scReal.real(filepath.Join(remoteLink, "new"))
	if !scReal.unknown {
		t.Fatal("real through remote parent symlink did not mark unknown")
	}

	// 9-hop symlink chain (> maxSymlinkHops) fails closed as unknown.
	chainPrev := filepath.Join(dir, "chain-target")
	if err := os.MkdirAll(chainPrev, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range maxSymlinkHops + 1 {
		next := filepath.Join(dir, "hop-"+string(rune('a'+i)))
		if err := os.Symlink(chainPrev, next); err != nil {
			t.Fatal(err)
		}
		chainPrev = next
	}
	scHops := newScan(dir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	scHops.candidate(chainPrev, plain)
	if !scHops.unknown {
		t.Fatal("9-hop symlink chain did not fail closed as unknown")
	}

	// Replacing a previously scanned parent directory with a remote symlink mid-scan still fails closed.
	swapDir := filepath.Join(dir, "swap")
	if err := os.MkdirAll(swapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(swapDir, "one"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	scSwap := newScan(dir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	scSwap.candidate(filepath.Join(swapDir, "one"), plain)
	if scSwap.unknown {
		t.Fatal("initial local file marked unknown")
	}
	if err := os.RemoveAll(swapDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(`//remote-server/share`, swapDir); err != nil {
		t.Fatal(err)
	}
	scSwap.candidate(filepath.Join(swapDir, "two"), plain)
	if !scSwap.unknown {
		t.Fatal("directory replaced with remote symlink mid-scan did not fail closed")
	}

	// Unquoted $BIG (500 KiB of "x "), fallbacks, and global replacements fail closed before expand.Fields materializes fields.
	bigFields := strings.Repeat("x ", 250_000)
	bigColon := strings.Repeat("x:", 25_000)
	tenKA := strings.Repeat("a", 10_000)
	fortyWords := strings.Repeat("x ", 40)
	envFields := func(k string) (string, bool) {
		switch k {
		case "BIG":
			return bigFields, true
		case "COLON":
			return bigColon, true
		case "VAR":
			return "a", true
		case "V":
			return tenKA, true
		case "REP":
			return fortyWords, true
		}
		return "", false
	}
	for _, cmd := range []string{
		"echo $BIG",
		"echo ${UNSET:-$BIG}",
		`echo ${UNSET:-"$BIG"}`,
		`echo ${VAR/a/"$BIG"}`,
		`echo ${V//a/` + fortyWords + `}`,
		`echo ${V//a/"$REP"}`,
	} {
		fBig, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		scF := newScan(dir, envFields, work{deadline: start.Add(time.Minute)})
		if _, err := scF.fields(fBig.Stmts[0].Cmd.(*syntax.CallExpr).Args[1]); err == nil || !scF.unknown {
			t.Fatalf("%q did not fail closed: err=%v unknown=%v", cmd, err, scF.unknown)
		}
		if took := time.Since(start); took > 50*time.Millisecond {
			t.Fatalf("%q took %v, want < 50ms", cmd, took)
		}
	}
	// Custom non-whitespace IFS (:) is also bounded.
	fColon, err := syntax.NewParser().Parse(strings.NewReader("echo $COLON"), "")
	if err != nil {
		t.Fatal(err)
	}
	scColon := newScan(dir, func(k string) (string, bool) {
		if k == "IFS" {
			return ":", true
		}
		return envFields(k)
	}, work{deadline: time.Now().Add(time.Minute)})
	if _, err := scColon.fields(fColon.Stmts[0].Cmd.(*syntax.CallExpr).Args[1]); err == nil || !scColon.unknown {
		t.Fatal("IFS=: $COLON did not fail closed")
	}

	// modcache matches Go: a leading empty GOPATH segment (e.g. ":/x") yields no GOMODCACHE.
	gpDir := filepath.Join(dir, "gp")
	scGP := newScan(dir, func(k string) (string, bool) {
		if k == "GOPATH" {
			return string(os.PathListSeparator) + gpDir, true
		}
		return "", false
	}, work{deadline: time.Now().Add(time.Minute)})
	if got, ok := modcache(scGP, nil); ok || got != "" {
		t.Fatalf("modcache(leading empty GOPATH) = (%q, %v), want (\"\", false)", got, ok)
	}

	// candidate cache keys ~ by HOME so changing HOME reaches both directories.
	homeA, homeB := filepath.Join(dir, "homeA"), filepath.Join(dir, "homeB")
	for _, h := range []string{homeA, homeB} {
		if err := os.MkdirAll(h, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	scHome := newScan(dir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	scHome.vars["HOME"] = &homeA
	scHome.candidate("~", plain)
	scHome.vars["HOME"] = &homeB
	scHome.candidate("~", plain)
	if !slices.Contains(scHome.out, homeA) || !slices.Contains(scHome.out, homeB) {
		t.Fatalf("~ cache missed HOME change: out=%q, want both %q and %q", scHome.out, homeA, homeB)
	}

	// link/../file resolves symlink before collapsing .. (both local target and remote fail-closed).
	otherParent := filepath.Join(dir, "other-parent")
	otherChild := filepath.Join(otherParent, "sub")
	otherFile := filepath.Join(otherParent, "secret.txt")
	if err := os.MkdirAll(otherChild, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	localLink := filepath.Join(dir, "local-link")
	if err := os.Symlink(otherChild, localLink); err != nil {
		t.Fatal(err)
	}
	scDotDot := newScan(dir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	resolvedParent, _ := filepath.EvalSymlinks(otherParent)
	if got, ok := scDotDot.path("local-link/../secret.txt", dir); !ok || got != filepath.Join(resolvedParent, "secret.txt") {
		t.Fatalf("path(local-link/../secret.txt) = (%q, %v), want %q", got, ok, filepath.Join(resolvedParent, "secret.txt"))
	}
	scRemoteDotDot := newScan(dir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	if _, ok := scRemoteDotDot.path(filepath.Base(remoteLink)+"/../file", dir); ok || !scRemoteDotDot.unknown {
		t.Fatalf("remoteLink/../file did not fail closed: ok=%v unknown=%v", ok, scRemoteDotDot.unknown)
	}

	// Large unknown command substitution dispatches in O(1) without quadratic prefix joins.
	bigSubst := "echo $(unknown" + strings.Repeat(" a", 4000) + ")"
	fSubst, err := syntax.NewParser().Parse(strings.NewReader(bigSubst), "")
	if err != nil {
		t.Fatal(err)
	}
	cs := fSubst.Stmts[0].Cmd.(*syntax.CallExpr).Args[1].Parts[0].(*syntax.CmdSubst)
	startSubst := time.Now()
	scSubst := newScan(dir, os.LookupEnv, work{deadline: startSubst.Add(time.Minute)})
	var sb strings.Builder
	if err := scSubst.substitute(&sb, cs); !errors.Is(err, errOpaque) {
		t.Fatalf("large unknown substitute err = %v, want errOpaque", err)
	}
	if took := time.Since(startSubst); took > 50*time.Millisecond {
		t.Fatalf("large unknown substitute took %v, want < 50ms", took)
	}

	// Whitespace-heavy known command substitution fails closed before expand.Fields materializes fields.
	hugeGP := filepath.Join(dir, strings.Repeat("x ", 250_000))
	fGP, err := syntax.NewParser().Parse(strings.NewReader("echo $(go env GOPATH)"), "")
	if err != nil {
		t.Fatal(err)
	}
	startGP := time.Now()
	scGopath := newScan(dir, func(k string) (string, bool) {
		if k == "GOPATH" {
			return hugeGP, true
		}
		return "", false
	}, work{deadline: startGP.Add(time.Minute)})
	if _, err := scGopath.fields(fGP.Stmts[0].Cmd.(*syntax.CallExpr).Args[1]); err == nil || !scGopath.unknown {
		t.Fatalf("whitespace-heavy $(go env GOPATH) did not fail closed: err=%v unknown=%v", err, scGopath.unknown)
	}
	if took := time.Since(startGP); took > 50*time.Millisecond {
		t.Fatalf("whitespace-heavy $(go env GOPATH) took %v, want < 50ms", took)
	}

	// Dangling symlink (out -> ../api/new.txt) records the target's nearest existing parent.
	apiDir := filepath.Join(dir, "dangling-api")
	sideDir := filepath.Join(dir, "dangling-side")
	for _, d := range []string{apiDir, sideDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join("..", "dangling-api", "new.txt"), filepath.Join(sideDir, "out")); err != nil {
		t.Fatal(err)
	}
	scDang := newScan(sideDir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	scDang.candidate("out", creates)
	if scDang.unknown || !slices.Contains(scDang.out, apiDir) {
		t.Fatalf("dangling symlink out -> ../dangling-api/new.txt: out=%q unknown=%v, want %q", scDang.out, scDang.unknown, apiDir)
	}

	// Symlink target with ".." past a remote symlink (out -> jump/../file, jump -> //host/share/subdir) fails closed.
	jumpLink := filepath.Join(dir, "jump")
	if err := os.Symlink("//host/share/subdir", jumpLink); err != nil {
		t.Fatal(err)
	}
	bypassLink := filepath.Join(dir, "bypass-out")
	if err := os.Symlink("jump/../file", bypassLink); err != nil {
		t.Fatal(err)
	}
	scBypass := newScan(dir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	scBypass.candidate(bypassLink, creates)
	if !scBypass.unknown {
		t.Fatal("symlink target jump/../file through remote jump did not fail closed")
	}

	// Remote GIT_DIR and .git file gitdir targets fail closed before LocateFS stats them.
	fGit, err := syntax.NewParser().Parse(strings.NewReader("echo $(git rev-parse --show-toplevel)"), "")
	if err != nil {
		t.Fatal(err)
	}
	csGit := fGit.Stmts[0].Cmd.(*syntax.CallExpr).Args[1].Parts[0].(*syntax.CmdSubst)
	scGitEnv := newScan(dir, func(k string) (string, bool) {
		if k == "GIT_DIR" {
			return "//server/share/repo.git", true
		}
		return "", false
	}, work{deadline: time.Now().Add(time.Minute)})
	var gitBuf strings.Builder
	if err := scGitEnv.substitute(&gitBuf, csGit); err == nil || !scGitEnv.unknown {
		t.Fatalf("remote GIT_DIR did not fail closed: err=%v unknown=%v", err, scGitEnv.unknown)
	}
	gitFileDir := filepath.Join(dir, "remote-gitfile")
	if err := os.MkdirAll(gitFileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitFileDir, ".git"), []byte("gitdir: //server/share/repo.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scGitFile := newScan(gitFileDir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	if err := scGitFile.substitute(&gitBuf, csGit); err == nil || !scGitFile.unknown {
		t.Fatalf("remote .git gitdir file did not fail closed: err=%v unknown=%v", err, scGitFile.unknown)
	}
	// Scan-provided local GIT_DIR/GIT_WORK_TREE resolves consistently using scan's env.
	customWT := filepath.Join(dir, "custom-wt")
	customGD := filepath.Join(dir, "custom-gd")
	for _, d := range []string{customWT, customGD} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	scGitCustom := newScan(dir, func(k string) (string, bool) {
		switch k {
		case "GIT_DIR":
			return customGD, true
		case "GIT_WORK_TREE":
			return customWT, true
		}
		return "", false
	}, work{deadline: time.Now().Add(time.Minute)})
	gitBuf.Reset()
	if err := scGitCustom.substitute(&gitBuf, csGit); err != nil || gitBuf.String() != customWT {
		t.Fatalf("scan GIT_DIR/GIT_WORK_TREE substitute = (%q, %v), want (%q, nil)", gitBuf.String(), err, customWT)
	}
	// Triple-slash path (///...) is resolved on POSIX (never silently dropped) while "// TODO" stays clean.
	if filepath.Separator == '/' {
		tripleFile := filepath.Join(apiDir, "triple.txt")
		if err := os.WriteFile(tripleFile, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		scTriple := newScan(sideDir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
		scTriple.candidate("//"+tripleFile, plain)
		if scTriple.unknown || !slices.Contains(scTriple.out, apiDir) {
			t.Fatalf("triple-slash path //%s: out=%q unknown=%v, want %q", tripleFile, scTriple.out, scTriple.unknown, apiDir)
		}
	}
	scComment := newScan(sideDir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	scComment.candidate("// TODO", plain)
	scComment.candidate("//", plain)
	if scComment.unknown {
		t.Fatal("comment-like // tokens should not mark unknown")
	}

	// Global replacement with command substitution (${V//a/$(go env GOPATH)}) fails closed before expand.Fields.
	fReplSubst, err := syntax.NewParser().Parse(strings.NewReader("echo ${V//a/$(go env GOPATH)}"), "")
	if err != nil {
		t.Fatal(err)
	}
	startRS := time.Now()
	scRS := newScan(dir, func(k string) (string, bool) {
		switch k {
		case "V":
			return strings.Repeat("a", 100), true
		case "GOPATH":
			return strings.Repeat("x ", 5000), true
		}
		return "", false
	}, work{deadline: startRS.Add(time.Minute)})
	if _, err := scRS.fields(fReplSubst.Stmts[0].Cmd.(*syntax.CallExpr).Args[1]); err == nil || !scRS.unknown {
		t.Fatalf("${V//a/$(go env GOPATH)} did not fail closed: err=%v unknown=%v", err, scRS.unknown)
	}
	if took := time.Since(startRS); took > 50*time.Millisecond {
		t.Fatalf("${V//a/$(go env GOPATH)} took %v, want < 50ms", took)
	}

	// Oversized path passed to hasRemoteLink/real fails closed rather than claiming safe.
	scBigPath := newScan(dir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	if !scBigPath.hasRemoteLink(filepath.Join(dir, strings.Repeat("a/", 2200)+"leaf")) || !scBigPath.unknown {
		t.Fatal("oversized path in hasRemoteLink did not fail closed")
	}

	// On Unix, backslash is a literal filename character and not a path separator for "..".
	if !os.IsPathSeparator('\\') {
		bsDir := filepath.Join(dir, `foo\..`)
		if err := os.MkdirAll(bsDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bsDir, "file"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		scBS := newScan(dir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
		if got, ok := scBS.path(`foo\../file`, dir); !ok || got != filepath.Join(bsDir, "file") {
			t.Fatalf("path(`foo\\../file`) = (%q, %v), want %q", got, ok, filepath.Join(bsDir, "file"))
		}
	}

	// GIT_WORK_TREE set without GIT_DIR (or unresolved in script vars) fails closed in safeLocate.
	wtOnlyRepo := filepath.Join(dir, "wt-only-repo")
	if err := os.MkdirAll(filepath.Join(wtOnlyRepo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	scWTOnly := newScan(wtOnlyRepo, func(k string) (string, bool) {
		if k == "GIT_WORK_TREE" {
			return customWT, true
		}
		return "", false
	}, work{deadline: time.Now().Add(time.Minute)})
	gitBuf.Reset()
	if err := scWTOnly.substitute(&gitBuf, csGit); err == nil {
		t.Fatalf("GIT_WORK_TREE without GIT_DIR resolved to %q, want errOpaque", gitBuf.String())
	}
	scUnresolvedGD := newScan(wtOnlyRepo, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	scUnresolvedGD.vars["GIT_DIR"] = nil
	if got, ok := scUnresolvedGD.safeLocate(wtOnlyRepo); ok {
		t.Fatalf("unresolved GIT_DIR resolved to %q, want false", got)
	}

	// Quoted or escaped secret flags and userinfo in firstWords are still masked.
	for _, tc := range []struct {
		cmd, leak string
	}{
		{`curl --us""er alice:supersecret https://example.com`, "supersecret"},
		{`curl -""u alice:supersecret https://example.com`, "supersecret"},
		{`curl \-u alice:supersecret https://example.com`, "supersecret"},
		{`curl --he""ader="Authorization: Bearer topsecret" https://example.com`, "topsecret"},
		{`curl -H"X-Api: key=topsecret" https://example.com`, "topsecret"},
		{`curl https://alice:supersecret"@"example.com/x`, "supersecret"},
		{`re""dis-cli -a topsecret ping`, "topsecret"},
		{`curl $'--password' hunter2 https://example.com`, "hunter2"},
		{`curl $'--user' alice:hunter2 https://example.com`, "hunter2"},
		{`curl $'\x2d\x2dpassword' hunter2 https://example.com`, "hunter2"},
		{`curl $'\055u' alice:hunter2 https://example.com`, "hunter2"},
		{`curl $'\u002dp' hunter2 https://example.com`, "hunter2"},
		{`curl $"--user" alice:hunter2 https://example.com`, "hunter2"},
		{`curl $'--user=alice:hunter2' https://example.com`, "hunter2"},
		{`curl $'-u'hunter2 https://example.com`, "hunter2"},
		{"curl --us\\\ner alice:hunter2 https://example.com", "hunter2"},
		{"curl --us\\\r\ner alice:hunter2 https://example.com", "hunter2"},
		{"curl \"--us\\\ner\" alice:hunter2 https://example.com", "hunter2"},
		{"curl \\\n  --user alice:hunter2 https://example.com", "hunter2"},
		{"curl $\\\n'--user' alice:hunter2 https://example.com", "hunter2"},
		{`curl $'--user\x00ignored' alice:supersecret https://example.com`, "supersecret"},
		{`curl $'--us\0ignored''er' alice:supersecret https://example.com`, "supersecret"},
		{`curl $'--password\c@ignored' hunter2 https://example.com`, "hunter2"},
	} {
		if got := firstWords(tc.cmd); strings.Contains(got, tc.leak) {
			t.Errorf("firstWords(%q) = %q, leaked %q", tc.cmd, got, tc.leak)
		}
	}

	// Quoted global replacement with CmdSubst ("${V//a/$(go env GOPATH)}") and oversized Repl.Orig (${V//$PAT/x}) fail closed.
	for _, cmd := range []string{`echo "${V//a/$(go env GOPATH)}"`, `echo "${V//$PAT/x}"`} {
		fRepl, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
		if err != nil {
			t.Fatal(err)
		}
		scRepl := newScan(dir, func(k string) (string, bool) {
			switch k {
			case "V":
				return strings.Repeat("a", 200), true
			case "GOPATH", "PAT":
				return strings.Repeat("x", 64<<10), true
			}
			return "", false
		}, work{deadline: time.Now().Add(time.Minute)})
		if _, err := scRepl.fields(fRepl.Stmts[0].Cmd.(*syntax.CallExpr).Args[1]); err == nil || !scRepl.unknown {
			t.Fatalf("%s did not fail closed: err=%v unknown=%v", cmd, err, scRepl.unknown)
		}
	}

	// pwd -P resolves symlinked directories physically while pwd -L keeps the logical path.
	realSub := filepath.Join(dir, "real-sub")
	linkSub := filepath.Join(dir, "link-sub")
	if err := os.MkdirAll(realSub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realSub, linkSub); err != nil {
		t.Fatal(err)
	}
	resolvedRealSub, _ := filepath.EvalSymlinks(realSub)
	scPwd := newScan(linkSub, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	if got, ok := outputs["pwd -P"](scPwd, nil); !ok || got != resolvedRealSub {
		t.Fatalf("pwd -P = (%q, %v), want (%q, true)", got, ok, resolvedRealSub)
	}
	if got, ok := where(scPwd, nil); !ok || got != linkSub {
		t.Fatalf("pwd -L = (%q, %v), want (%q, true)", got, ok, linkSub)
	}

	// parsable rejects oversized single-word shell payloads before scanning or parsing.
	if parsable(strings.Repeat("a", maxShellBytes+1)) {
		t.Fatal("parsable accepted source exceeding maxShellBytes")
	}

	// On Windows, a rooted volume-less path (\other\repo) resolves on the base volume, while a drive-relative path (C:../api) fails closed.
	if os.IsPathSeparator('\\') {
		scWin := newScan(dir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
		want := filepath.VolumeName(dir) + `\other\repo\file`
		if got, ok := scWin.resolveTarget(dir, `\other\repo\file`, 0); !ok || got != want {
			t.Fatalf(`resolveTarget(\other\repo\file) = (%q, %v), want (%q, true)`, got, ok, want)
		}
		if got, ok := scWin.resolveTarget(dir, `C:..\api\file`, 0); ok || !scWin.unknown {
			t.Fatalf(`resolveTarget(C:..\api\file) = (%q, %v) unknown=%v, want ("", false, true)`, got, ok, scWin.unknown)
		}
	}

	// Nested shell commands and curl --data-ascii/--json in firstWords recursively mask secrets while still masking NAME="value with space".
	for _, tc := range []struct {
		cmd, leak string
	}{
		{`bash -c 'curl -u alice:supersecret https://example.com'`, "supersecret"},
		{`bash -c 'echo hi; TOKEN=supersecret'`, "supersecret"},
		{`bash -c 'curl --header "Authorization: Bearer supersecret"'`, "supersecret"},
		{`sh -c 'bash -c "eval \"curl -u alice:supersecret\""'`, "supersecret"},
		{`curl --data-ascii supersecret https://example.com`, "supersecret"},
		{`curl --json '{"token":"supersecret"}' https://example.com`, "supersecret"},
		{`TOKEN="secret value" cmd`, "secret"},
	} {
		if got := firstWords(tc.cmd); strings.Contains(got, tc.leak) {
			t.Errorf("firstWords(%q) = %q, leaked %q", tc.cmd, got, tc.leak)
		}
	}
	if got := firstWords(`TOKEN="secret value" cmd`); got != "TOKEN=… cmd" {
		t.Errorf(`firstWords(TOKEN="secret value" cmd) = %q, want "TOKEN=… cmd"`, got)
	}

	// Explicit GIT_DIR without GIT_WORK_TREE fails closed instead of assuming cwd is the worktree.
	scGDOnly := newScan(wtOnlyRepo, func(k string) (string, bool) {
		if k == "GIT_DIR" {
			return customGD, true
		}
		return "", false
	}, work{deadline: time.Now().Add(time.Minute)})
	gitBuf.Reset()
	if err := scGDOnly.substitute(&gitBuf, csGit); err == nil {
		t.Fatalf("GIT_DIR without GIT_WORK_TREE resolved to %q, want errOpaque", gitBuf.String())
	}

	// Parameter escape transformation (${V@E}) fails closed before expand.Fields materializes decoded fields.
	fTransform, err := syntax.NewParser().Parse(strings.NewReader("echo ${V@E}"), "")
	if err != nil {
		t.Fatal(err)
	}
	startTr := time.Now()
	scTr := newScan(dir, func(k string) (string, bool) {
		if k == "V" {
			return strings.Repeat(`x\t`, 250000), true
		}
		return "", false
	}, work{deadline: startTr.Add(time.Minute)})
	if _, err := scTr.fields(fTransform.Stmts[0].Cmd.(*syntax.CallExpr).Args[1]); err == nil || !scTr.unknown {
		t.Fatalf("${V@E} did not fail closed: err=%v unknown=%v", err, scTr.unknown)
	}
	if took := time.Since(startTr); took > 50*time.Millisecond {
		t.Fatalf("${V@E} took %v, want < 50ms", took)
	}

	// hasRemoteLink revalidates cwd on every call rather than caching an earlier local check.
	if !os.IsPathSeparator('\\') {
		localCwdTarget := filepath.Join(dir, "cwd-local")
		swappableCwd := filepath.Join(dir, "cwd-link")
		if err := os.MkdirAll(localCwdTarget, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(localCwdTarget, swappableCwd); err != nil {
			t.Fatal(err)
		}
		scSwap := newScan(swappableCwd, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
		if scSwap.hasRemoteLink(swappableCwd) || scSwap.unknown {
			t.Fatalf("initial local cwd symlink flagged remote: unknown=%v", scSwap.unknown)
		}
		if err := os.Remove(swappableCwd); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("//remote/share", swappableCwd); err != nil {
			t.Fatal(err)
		}
		if !scSwap.hasRemoteLink(swappableCwd) || !scSwap.unknown {
			t.Fatalf("swapped remote cwd symlink was not detected: unknown=%v", scSwap.unknown)
		}
	}

	// real (realpath / readlink -f) resolves nonexistent leaf under existing directory, but fails closed on missing parent, file parent, or dangling symlink.
	realParent := filepath.Join(dir, "real-parent")
	realFile := filepath.Join(realParent, "plain.txt")
	if err := os.MkdirAll(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(realFile, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolvedRealParent, _ := filepath.EvalSymlinks(realParent)
	scRealLeaf := newScan(dir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	if got, ok := scRealLeaf.real(filepath.Join(realParent, "new.txt")); !ok || got != filepath.Join(resolvedRealParent, "new.txt") {
		t.Fatalf("real(existing/new.txt) = (%q, %v), want (%q, true)", got, ok, filepath.Join(resolvedRealParent, "new.txt"))
	}
	for _, bad := range []string{
		filepath.Join(realParent, "missing", "new.txt"),
		filepath.Join(realFile, "new.txt"),
	} {
		if got, ok := scRealLeaf.real(bad); ok {
			t.Fatalf("real(%q) = (%q, true), want false", bad, got)
		}
	}
	if !os.IsPathSeparator('\\') {
		dangling := filepath.Join(realParent, "dangling")
		if err := os.Symlink(filepath.Join(realParent, "does-not-exist"), dangling); err != nil {
			t.Fatal(err)
		}
		wantDangling := filepath.Join(resolvedRealParent, "does-not-exist")
		if got, ok := scRealLeaf.real(dangling); !ok || got != wantDangling {
			t.Fatalf("real(dangling -> existing/does-not-exist) = (%q, %v), want (%q, true)", got, ok, wantDangling)
		}
		danglingDeep := filepath.Join(realParent, "dangling-deep")
		if err := os.Symlink(filepath.Join(realParent, "missing", "does-not-exist"), danglingDeep); err != nil {
			t.Fatal(err)
		}
		if got, ok := scRealLeaf.real(danglingDeep); ok {
			t.Fatalf("real(dangling -> missing/does-not-exist) = (%q, true), want false", got)
		}
	}

	// hasGoEnv only blocks GOPATH/GOMODCACHE when go/env actually overrides that key; on Windows, GOPATH defaults from USERPROFILE.
	goEnvBenign := filepath.Join(dir, "go-env-benign")
	goEnvOverride := filepath.Join(dir, "go-env-override")
	if err := os.WriteFile(goEnvBenign, []byte("GOTOOLCHAIN=auto\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goEnvOverride, []byte("GOPATH=/custom/gopath\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scGoBenign := newScan(dir, func(k string) (string, bool) {
		switch k {
		case "HOME", "USERPROFILE":
			return dir, true
		case "GOENV":
			return goEnvBenign, true
		}
		return "", false
	}, work{deadline: time.Now().Add(time.Minute)})
	if got, ok := gopath(scGoBenign, nil); !ok || got != filepath.Join(dir, "go") {
		t.Fatalf("gopath with benign go/env = (%q, %v), want (%q, true)", got, ok, filepath.Join(dir, "go"))
	}
	scGoOverride := newScan(dir, func(k string) (string, bool) {
		switch k {
		case "HOME", "USERPROFILE":
			return dir, true
		case "GOENV":
			return goEnvOverride, true
		}
		return "", false
	}, work{deadline: time.Now().Add(time.Minute)})
	if got, ok := gopath(scGoOverride, nil); ok {
		t.Fatalf("gopath with GOPATH override in go/env = (%q, true), want false", got)
	}
	if os.IsPathSeparator('\\') {
		winProfile := filepath.Join(dir, "win-profile")
		scWinGP := newScan(dir, func(k string) (string, bool) {
			switch k {
			case "HOME":
				return filepath.Join(dir, "msys-home"), true
			case "USERPROFILE":
				return winProfile, true
			case "GOENV":
				return "off", true
			}
			return "", false
		}, work{deadline: time.Now().Add(time.Minute)})
		if got, ok := gopath(scWinGP, nil); !ok || got != filepath.Join(winProfile, "go") {
			t.Fatalf("windows gopath = (%q, %v), want (%q, true)", got, ok, filepath.Join(winProfile, "go"))
		}
	}

	// Remote HOME with ~ or ~/... fails closed (sc.unknown = true) without statting.
	scRemoteHome := newScan(dir, func(k string) (string, bool) {
		if k == "HOME" {
			return "//host/share", true
		}
		return "", false
	}, work{deadline: time.Now().Add(time.Minute)})
	if _, ok := scRemoteHome.path("~/repo/file", dir); ok || !scRemoteHome.unknown {
		t.Fatalf("path(~/repo/file) with remote HOME: ok=%v unknown=%v, want (false, true)", ok, scRemoteHome.unknown)
	}

	// Non-cwd hasRemoteLink and safeLocate both charge the shared maxShellStats budget.
	budgetDir := filepath.Join(dir, "budget-a", "budget-b", "budget-c")
	if err := os.MkdirAll(budgetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	scBudgetLink := newScan(dir, os.LookupEnv, work{stats: maxShellStats - 1, deadline: time.Now().Add(time.Minute)})
	if !scBudgetLink.hasRemoteLink(budgetDir) || !scBudgetLink.unknown {
		t.Fatalf("hasRemoteLink near stat limit did not exhaust budget: unknown=%v stats=%d", scBudgetLink.unknown, scBudgetLink.stats)
	}
	scBudgetLoc := newScan(dir, os.LookupEnv, work{stats: maxShellStats - 1, deadline: time.Now().Add(time.Minute)})
	if _, ok := scBudgetLoc.safeLocate(budgetDir); ok || !scBudgetLoc.unknown {
		t.Fatalf("safeLocate near stat limit did not exhaust budget: ok=%v unknown=%v stats=%d", ok, scBudgetLoc.unknown, scBudgetLoc.stats)
	}
}

func parseFirstArg(t *testing.T, cmd string) *syntax.Word {
	t.Helper()
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(cmd), "")
	if err != nil {
		t.Fatalf("parse %q: %v", cmd, err)
	}
	return f.Stmts[0].Cmd.(*syntax.CallExpr).Args[1]
}

func TestReachCoreHardeningRegressions(t *testing.T) {
	// 1. firstWords preserves secret-option masking across attached/spaced redirections and masks here-strings.
	for _, tc := range []struct {
		cmd, leaked string
	}{
		{"curl --user</dev/null alice:LEAK https://example.com", "LEAK"},
		{"curl --user < /dev/null alice:LEAK https://example.com", "LEAK"},
		{"curl --user 2>/dev/null alice:LEAK https://example.com", "LEAK"},
		{"curl -u<<<LEAK https://example.com", "LEAK"},
		{"curl -u <<< LEAK https://example.com", "LEAK"},
	} {
		if got := firstWords(tc.cmd); strings.Contains(got, tc.leaked) {
			t.Fatalf("firstWords(%q) = %q, leaked %q", tc.cmd, got, tc.leaked)
		}
	}

	dir := t.TempDir()

	// 2. hasRemoteLink / fileHasLine / hasGoEnv check ".." segments before cleaning so /safe/jump/../env with remote jump fails closed.
	if !os.IsPathSeparator('\\') {
		jump := filepath.Join(dir, "jump")
		if err := os.Symlink("//remote/share", jump); err != nil {
			t.Fatal(err)
		}
		tricky := jump + string(filepath.Separator) + ".." + string(filepath.Separator) + "go-env"
		if err := os.WriteFile(filepath.Join(dir, "go-env"), []byte("GOTOOLCHAIN=auto\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		scJump := newScan(dir, func(k string) (string, bool) {
			switch k {
			case "HOME", "USERPROFILE":
				return dir, true
			case "GOENV":
				return tricky, true
			}
			return "", false
		}, work{deadline: time.Now().Add(time.Minute)})
		if !scJump.hasRemoteLink(tricky) || !scJump.unknown {
			t.Fatalf("hasRemoteLink(%q) did not fail closed on remote symlink before ..", tricky)
		}
		scGoJump := newScan(dir, func(k string) (string, bool) {
			switch k {
			case "HOME", "USERPROFILE":
				return dir, true
			case "GOENV":
				return tricky, true
			}
			return "", false
		}, work{deadline: time.Now().Add(time.Minute)})
		if got, ok := gopath(scGoJump, nil); ok {
			t.Fatalf("gopath with GOENV=%q = (%q, true), want false", tricky, got)
		}
	}

	// 3. realpath '' and readlink -f '' reject empty operands.
	for _, cmd := range []string{"echo $(realpath '')", "echo $(readlink -f '')"} {
		w := parseFirstArg(t, cmd)
		sc := newScan(dir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
		if got, err := sc.fields(w); err == nil {
			t.Fatalf("%s expanded to %q, want error", cmd, got)
		}
	}

	// 4. Huge IFS fails closed immediately without quadratic scanning.
	bigIFS := strings.Repeat(":", 65536)
	bigVal := strings.Repeat("a", 512*1024)
	wIFS := parseFirstArg(t, "echo $V")
	scIFS := newScan(dir, func(k string) (string, bool) {
		switch k {
		case "IFS":
			return bigIFS, true
		case "V":
			return bigVal, true
		}
		return "", false
	}, work{deadline: time.Now().Add(time.Minute)})
	start := time.Now()
	if got, err := scIFS.fields(wIFS); err == nil || !scIFS.unknown {
		t.Fatalf("huge IFS expanded to %q (err=%v, unknown=%v), want opaque fail-closed", got, err, scIFS.unknown)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("huge IFS took %v, want <100ms", d)
	}

	// 5. Large removal (##) and case-conversion (^^) patterns fail closed quickly.
	pat := strings.Repeat("a", 4097)
	val := strings.Repeat("a", 50000)
	for _, expr := range []string{"echo ${V##$PAT}", "echo ${V^^$PAT}", "echo ${V//$PAT/x}"} {
		w := parseFirstArg(t, expr)
		sc := newScan(dir, func(k string) (string, bool) {
			switch k {
			case "V":
				return val, true
			case "PAT":
				return pat, true
			}
			return "", false
		}, work{deadline: time.Now().Add(time.Minute)})
		t0 := time.Now()
		if got, err := sc.fields(w); err == nil || !sc.unknown {
			t.Fatalf("%s expanded to %q (err=%v, unknown=%v), want opaque fail-closed", expr, got, err, sc.unknown)
		}
		if d := time.Since(t0); d > 100*time.Millisecond {
			t.Fatalf("%s took %v, want <100ms", expr, d)
		}
	}

	// 6. GIT_CEILING_DIRECTORIES (set or unresolved) and unresolved GIT_DISCOVERY_ACROSS_FILESYSTEM fail closed in safeLocate.
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	scCeil := newScan(repo, func(k string) (string, bool) {
		if k == "GIT_CEILING_DIRECTORIES" {
			return dir, true
		}
		return "", false
	}, work{deadline: time.Now().Add(time.Minute)})
	if got, ok := scCeil.safeLocate(repo); ok {
		t.Fatalf("safeLocate with GIT_CEILING_DIRECTORIES = (%q, true), want false", got)
	}
	scUnres := newScan(repo, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	scUnres.vars["GIT_DISCOVERY_ACROSS_FILESYSTEM"] = nil
	if got, ok := scUnres.safeLocate(repo); ok {
		t.Fatalf("safeLocate with unresolved GIT_DISCOVERY_ACROSS_FILESYSTEM = (%q, true), want false", got)
	}

	// 7. core.worktree in .git/config or gitdir-target/config fails closed in safeLocate.
	wtRepo := filepath.Join(dir, "wt-dir-repo")
	if err := os.MkdirAll(filepath.Join(wtRepo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtRepo, ".git", "config"), []byte("[core]\n\tworktree = /elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scWTDir := newScan(wtRepo, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
	if got, ok := scWTDir.safeLocate(wtRepo); ok {
		t.Fatalf("safeLocate on .git dir with core.worktree = (%q, true), want false", got)
	}
	wtFileRepo := filepath.Join(dir, "wt-file-repo")
	wtExtGit := filepath.Join(dir, "ext-gitdir")
	if err := os.MkdirAll(wtFileRepo, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wtExtGit, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtFileRepo, ".git"), []byte("gitdir: "+wtExtGit+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtExtGit, "config"), []byte("[core]\n\tworktree = /elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scWTFile := newScan(wtFileRepo, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
	if got, ok := scWTFile.safeLocate(wtFileRepo); ok {
		t.Fatalf("safeLocate on .git file with core.worktree = (%q, true), want false", got)
	}

	// 8. mktemp rejects a second template operand or a template without XXX.
	for _, cmd := range []string{"echo $(mktemp foo.XXX bar.XXX)", "echo $(mktemp abc)"} {
		wMktemp := parseFirstArg(t, cmd)
		scMktemp := newScan(dir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
		if got, err := scMktemp.fields(wMktemp); err == nil {
			t.Fatalf("%s expanded to %q, want error", cmd, got)
		}
	}

	// 9. firstWords masks command/process substitutions, -c/eval payloads, and ${/$IFS expansions.
	for _, cmd := range []string{
		`curl -u $(echo alice:SECRET) https://example.com`,
		"curl -u `echo alice:SECRET` https://example.com",
		`echo $(curl -u alice:SECRET https://example.com)`,
		"echo `curl -u alice:SECRET https://example.com`",
		`echo $(curl${IFS}-u${IFS}alice:SECRET)`,
		"echo `curl${IFS}-u${IFS}alice:SECRET`",
		`echo >$(curl -u alice:SECRET)`,
		`echo > $(curl -u alice:SECRET)`,
		"echo >`curl -u alice:SECRET`",
		`cat <(curl -u alice:SECRET https://example.com)`,
		`echo hi >(curl -u alice:SECRET https://example.com)`,
		`bash -c 'curl${IFS}--user${IFS}alice:SECRET'`,
		`bash -c '{curl,--user,alice:SECRET}'`,
		`eval '{curl,--user,alice:SECRET}'`,
		`curl${IFS}-u${IFS}alice:SECRET`,
	} {
		if got := firstWords(cmd); strings.Contains(got, "SECRET") {
			t.Errorf("firstWords(%q) = %q, leaked SECRET", cmd, got)
		}
	}

	// 10. gopath and modcache fail closed on relative GOPATH entries or relative GOMODCACHE.
	for _, tc := range []struct {
		name string
		env  map[string]string
		fn   func(*scan, []*syntax.Word) (string, bool)
	}{
		{"relative GOPATH", map[string]string{"GOPATH": "relative"}, gopath},
		{"mixed GOPATH", map[string]string{"GOPATH": dir + string(os.PathListSeparator) + "relative"}, gopath},
		{"relative GOMODCACHE", map[string]string{"GOMODCACHE": "relative/mod"}, modcache},
	} {
		scRel := newScan(dir, func(k string) (string, bool) { v, ok := tc.env[k]; return v, ok }, work{deadline: time.Now().Add(time.Minute)})
		if got, ok := tc.fn(scRel, nil); ok {
			t.Fatalf("%s = (%q, true), want false", tc.name, got)
		}
	}

	// 11. safeLocate fails closed when GIT_DISCOVERY_ACROSS_FILESYSTEM is set, and sameDevice agrees on same filesystem.
	scAcross := newScan(repo, func(k string) (string, bool) {
		if k == "GIT_DISCOVERY_ACROSS_FILESYSTEM" {
			return "true", true
		}
		return "", false
	}, work{deadline: time.Now().Add(time.Minute)})
	if got, ok := scAcross.safeLocate(repo); ok {
		t.Fatalf("safeLocate with GIT_DISCOVERY_ACROSS_FILESYSTEM=true = (%q, true), want false", got)
	}
	infoA, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	infoB, err := os.Stat(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !sameDevice(infoA, infoB) {
		t.Fatal("sameDevice returned false for sibling directory on same filesystem")
	}

	// 12. safeLocate resolves symlinked directories physically before ascending (repoA/link -> repoB/sub discovers repoB).
	repoA := filepath.Join(dir, "repoA")
	repoB := filepath.Join(dir, "repoB")
	repoBSub := filepath.Join(repoB, "sub")
	for _, d := range []string{filepath.Join(repoA, ".git"), filepath.Join(repoB, ".git"), repoBSub} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	crossLink := filepath.Join(repoA, "link")
	if err := os.Symlink(repoBSub, crossLink); err != nil {
		t.Fatal(err)
	}
	resolvedRepoB, err := filepath.EvalSymlinks(repoB)
	if err != nil {
		t.Fatal(err)
	}
	scCross := newScan(crossLink, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
	if got, ok := scCross.safeLocate(crossLink); !ok || !samePath(got, resolvedRepoB) {
		t.Fatalf("safeLocate(repoA/link -> repoB/sub) = (%q, %v), want (%q, true)", got, ok, resolvedRepoB)
	}

	// 13. core.worktree in .git/config.worktree, inline [core] worktree = ..., and [include] fail closed in safeLocate.
	for _, tc := range []struct {
		name, file, body string
	}{
		{"config.worktree", "config.worktree", "[core]\n\tworktree = /elsewhere\n"},
		{"inline-worktree", "config", "[core] worktree = /elsewhere\n"},
		{"include-section", "config", "[include]\n\tpath = /elsewhere\n"},
	} {
		rdir := filepath.Join(dir, "wt-"+tc.name)
		if err := os.MkdirAll(filepath.Join(rdir, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rdir, ".git", tc.file), []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		scWTCfg := newScan(rdir, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
		if got, ok := scWTCfg.safeLocate(rdir); ok {
			t.Fatalf("safeLocate(%s) = (%q, true), want false", tc.name, got)
		}
	}

	// 14. Arithmetic expansions, parameter indices, and parameter slices recursively bound nested parameter explosions.
	for _, expr := range []string{
		`echo $(( ${V//a/$V} ))`,
		`echo ${A[${V//a/$V}]}`,
		`echo ${A:${V//a/$V}:1}`,
		`echo ${A:0:${V//a/$V}}`,
	} {
		w := parseFirstArg(t, expr)
		sc := newScan(dir, func(k string) (string, bool) {
			switch k {
			case "V":
				return val, true
			case "A":
				return "hello", true
			}
			return "", false
		}, work{deadline: time.Now().Add(time.Minute)})
		t0 := time.Now()
		if got, err := sc.fields(w); err == nil || !sc.unknown {
			t.Fatalf("%s expanded to %q (err=%v, unknown=%v), want opaque fail-closed", expr, got, err, sc.unknown)
		}
		if d := time.Since(t0); d > 100*time.Millisecond {
			t.Fatalf("%s took %v, want <100ms", expr, d)
		}
	}

	// 15. Quoted or escaped secret values starting with '<' or '>' are not mistaken for redirections.
	for _, cmd := range []string{
		`cli --password '<SECRET>'`,
		`cli --password "<SECRET>"`,
		`cli --password \<SECRET>`,
		`cli --password $'<SECRET>'`,
		`cli --password '>SECRET'`,
	} {
		if got := firstWords(cmd); strings.Contains(got, "SECRET") {
			t.Fatalf("firstWords(%q) = %q, leaked quoted angle-bracket secret", cmd, got)
		}
	}

	// 16. Schemeless URL userinfo (user:pass@host, :pass@host) is masked without breaking git scp specs.
	for _, cmd := range []string{
		`curl user:SECRET@example.com/path`,
		`curl :SECRET@example.com/path`,
		`curl 'user:SECRET@example.com/path'`,
		`curl >user:SECRET@example.com/path`,
	} {
		if got := firstWords(cmd); strings.Contains(got, "SECRET") {
			t.Fatalf("firstWords(%q) = %q, leaked schemeless URL userinfo", cmd, got)
		}
	}
	if got := firstWords(`git clone git@github.com:org/repo`); !strings.Contains(got, "git@github.com:org/repo") {
		t.Fatalf("firstWords(git scp) = %q, unexpectedly masked git scp target", got)
	}

	// 17. Paths exceeding maxPathBytes fail closed (sc.unknown = true) in reach, placeParent, and cleanSafeAt.
	longRel := strings.Repeat("a/", maxPathBytes/2+10) + "leaf"
	scLong := newScan(dir, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
	scLong.candidate(longRel, plain)
	if !scLong.unknown {
		t.Fatalf("oversized candidate failed to set unknown=true")
	}
	scLongDot := newScan(dir, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
	scLongDot.candidate("../"+longRel, plain)
	if !scLongDot.unknown {
		t.Fatalf("oversized ../ candidate failed to set unknown=true")
	}

	// 18. Unresolved variables (sc.vars[k] == nil) fail closed in fallback/indirect/arith/index/slice expansions,
	// while genuinely unset variables still use :- defaults.
	ref := "DIR"
	for _, expr := range []string{
		`echo ${DIR:-/safe}/file`,
		`echo ${DIR-/safe}/file`,
		`echo ${DIR:+/safe}/file`,
		`echo ${!REF:-/safe}/file`,
		`echo $(( DIR + 1 ))/file`,
		`echo ${A[DIR]}/file`,
		`echo ${A:DIR:1}/file`,
	} {
		w := parseFirstArg(t, expr)
		scU := newScan(dir, func(k string) (string, bool) {
			if k == "A" {
				return "hello", true
			}
			return "", false
		}, work{deadline: time.Now().Add(time.Minute)})
		scU.vars["DIR"] = nil
		scU.vars["REF"] = &ref
		if got, err := scU.fields(w); err == nil {
			t.Fatalf("%s expanded to %q with unresolved DIR, want errOpaque", expr, got)
		}
	}
	scUnset := newScan(dir, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
	gotUnset, err := scUnset.fields(parseFirstArg(t, `echo ${UNSET:-/safe}/file`))
	if err != nil || len(gotUnset) != 1 || gotUnset[0] != "/safe/file" {
		t.Fatalf("unset :- default = (%q, %v), want ([\"/safe/file\"], nil)", gotUnset, err)
	}

	// 19. Bundled short flags containing -c (e.g. bash -ce, sh -xc) mask the command payload.
	for _, cmd := range []string{
		`bash -ce '{curl,--user,alice:SECRET}'`,
		`sh -xc 'curl -u alice:SECRET https://example.com'`,
		`zsh -cu 'echo SECRET'`,
	} {
		if got := firstWords(cmd); strings.Contains(got, "SECRET") {
			t.Fatalf("firstWords(%q) = %q, leaked bundled -c payload", cmd, got)
		}
	}

	// 20. Named-user tilde expansion (~someone/path) fails closed without calling os/user.Lookup.
	for _, expr := range []string{
		`echo ~nonexistent_user_xyz_mir708/repo/file`,
		`echo ${UNSET:-~nonexistent_user_xyz_mir708/repo/file}`,
	} {
		scTilde := newScan(dir, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
		if got, err := scTilde.fields(parseFirstArg(t, expr)); err == nil {
			t.Fatalf("%s expanded to %q, want errOpaque without user lookup", expr, got)
		}
	}

	// 21. Unresolved IFS, indirect arithmetic $((N)) -> unresolved DIR, and unresolved HOME fail closed.
	scUnresIFS := newScan(dir, func(k string) (string, bool) {
		if k == "A" {
			return "x y", true
		}
		return "", false
	}, work{deadline: time.Now().Add(time.Minute)})
	scUnresIFS.vars["IFS"] = nil
	if got, err := scUnresIFS.fields(parseFirstArg(t, `echo $A`)); err == nil {
		t.Fatalf("unresolved IFS expanded $A to %q, want errOpaque", got)
	}
	scArithInd := newScan(dir, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
	scArithInd.vars["N"] = &ref
	scArithInd.vars["DIR"] = nil
	if got, err := scArithInd.fields(parseFirstArg(t, `echo $((N))`)); err == nil {
		t.Fatalf("indirect arith $((N)) with unresolved DIR expanded to %q, want errOpaque", got)
	}
	scHomeUnres := newScan(dir, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
	scHomeUnres.vars["HOME"] = nil
	if got, err := scHomeUnres.fields(parseFirstArg(t, `echo ~/file`)); err == nil {
		t.Fatalf("unresolved HOME expanded ~/file to %q, want errOpaque", got)
	}

	// 22. After cd sub, create-capable relative operand still places sub/nested-repo even when cwd/nested-repo/file exists.
	baseDir := filepath.Join(dir, "create-cd")
	subDir := filepath.Join(baseDir, "sub")
	cwdNested := filepath.Join(baseDir, "nested-repo")
	subNested := filepath.Join(subDir, "nested-repo")
	for _, d := range []string{cwdNested, subNested} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cwdNested, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	scCD := newScan(baseDir, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
	scCD.dir = subDir
	scCD.candidate("nested-repo/file", creates)
	if !slices.Contains(scCD.out, subNested) {
		t.Fatalf("create target after cd missed %q: out=%q", subNested, scCD.out)
	}

	// 23. Concurrent parent directory <-> remote symlink swap race never follows remote share.
	if filepath.Separator == '/' {
		raceDir := filepath.Join(dir, "toctou-race")
		realMid := filepath.Join(raceDir, "real-mid")
		if err := os.MkdirAll(realMid, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(realMid, "leaf"), []byte("ok"), 0o600); err != nil {
			t.Fatal(err)
		}
		mid := filepath.Join(raceDir, "mid")
		remoteTmp := filepath.Join(raceDir, "remote-tmp")
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				select {
				case <-stop:
					return
				default:
					_ = os.Remove(mid)
					_ = os.Symlink(realMid, mid)
					_ = os.Remove(remoteTmp)
					_ = os.Symlink("//evil.invalid/share", remoteTmp)
					_ = os.Rename(remoteTmp, mid)
				}
			}
		}()
		for range 40 {
			scRace := newScan(raceDir, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
			scRace.candidate("mid/leaf", plain)
			for _, p := range scRace.out {
				if strings.Contains(p, "evil.invalid") {
					close(stop)
					<-done
					t.Fatalf("TOCTOU race followed remote symlink: out=%q", scRace.out)
				}
			}
		}
		close(stop)
		<-done
	}

	// 23. firstWords masks heredoc bodies and opaque option expansions (--us${EMPTY}er, $(echo --user)).
	for _, cmd := range []string{
		"cat << EOF\nSECRET\nEOF",
		"cat <<EOF\nSECRET\nEOF",
		"cat <<- 'EOF'\n\tSECRET\nEOF",
		"cat 0<<EOF\nSECRET\nEOF",
		`curl --us${EMPTY}er alice:SECRET https://example.com`,
		`curl --us$EMPTY alice:SECRET https://example.com`,
		`curl $(echo --user) alice:SECRET https://example.com`,
		`curl ''$EMPTY alice:SECRET https://example.com`,
	} {
		if got := firstWords(cmd); strings.Contains(got, "SECRET") {
			t.Fatalf("firstWords(%q) = %q, leaked SECRET", cmd, got)
		}
	}

	// 24. readSmallFile and readDir open through the validated dirHandle, failing closed on remote symlinks and never panicking on root dirs.
	if filepath.Separator == '/' {
		remLink := filepath.Join(dir, "remote-link")
		if err := os.Symlink("//evil.invalid/share", remLink); err != nil {
			t.Fatal(err)
		}
		scRemFile := newScan(dir, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
		if _, err := scRemFile.readSmallFile(remLink); !errors.Is(err, errOpaque) || !scRemFile.unknown {
			t.Fatalf("readSmallFile(remote symlink) = err=%v unknown=%v, want errOpaque + unknown", err, scRemFile.unknown)
		}
		scRemDir := newScan(dir, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
		if entries := scRemDir.readDir(remLink); len(entries) != 0 || !scRemDir.unknown {
			t.Fatalf("readDir(remote symlink) = (%d, unknown=%v), want (0, true)", len(entries), scRemDir.unknown)
		}
		scRoot := newScan(dir, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
		if _, err := scRoot.readSmallFile(string(filepath.Separator)); !errors.Is(err, errOpaque) {
			t.Fatalf("readSmallFile(/) = %v, want errOpaque", err)
		}
	}

	// 25. Brace masking, arithmetic/index/slice fail-closed, case-op IFS bound, percent-encoded file:// URLs, and missing/.. non-collapse.
	for _, cmd := range []string{
		`{curl,--user,alice:SECRET}`,
		`curl --us{e,a}r alice:SECRET https://example.com`,
		`curl --header{,s} Bearer:SECRET https://example.com`,
	} {
		if got := firstWords(cmd); strings.Contains(got, "SECRET") {
			t.Fatalf("firstWords(%q) = %q, leaked brace-expanded secret", cmd, got)
		}
	}
	if got := firstWords(`echo {}`); got != "echo {}" {
		t.Fatalf("firstWords(echo {}) = %q, want %q", got, "echo {}")
	}

	exprVal := "1+1"
	for _, expr := range []string{
		`echo ../repo$((N))/file`,
		`echo ../repo$((1+1))/file`,
		`echo ../repo${A:0:1}/file`,
		`echo ../repo${A[0]}/file`,
	} {
		scArith := newScan(dir, func(k string) (string, bool) {
			if k == "A" {
				return "hello", true
			}
			return "", false
		}, work{deadline: time.Now().Add(time.Minute)})
		scArith.vars["N"] = &exprVal
		if got, err := scArith.fields(parseFirstArg(t, expr)); err == nil || !scArith.unknown {
			t.Fatalf("%s expanded to %q (err=%v, unknown=%v), want opaque fail-closed", expr, got, err, scArith.unknown)
		}
	}

	caseVal := strings.Repeat("xa", 250000)
	scCaseIFS := newScan(dir, func(k string) (string, bool) {
		switch k {
		case "IFS":
			return "A", true
		case "V":
			return caseVal, true
		}
		return "", false
	}, work{deadline: time.Now().Add(time.Minute)})
	tCase := time.Now()
	if got, err := scCaseIFS.fields(parseFirstArg(t, `echo ${V^^}`)); err == nil || !scCaseIFS.unknown {
		t.Fatalf("case-op IFS blowup expanded to %d fields (err=%v, unknown=%v), want fail-closed", len(got), err, scCaseIFS.unknown)
	}
	if d := time.Since(tCase); d > 100*time.Millisecond {
		t.Fatalf("case-op IFS check took %v, want <100ms", d)
	}

	privDir := filepath.Join(dir, "private")
	if err := os.MkdirAll(privDir, 0o700); err != nil {
		t.Fatal(err)
	}
	privFile := filepath.Join(privDir, "README")
	if err := os.WriteFile(privFile, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	encURL := "file://" + filepath.ToSlash(dir) + "/priva%74e/README"
	if !strings.HasPrefix(filepath.ToSlash(dir), "/") {
		encURL = "file:///" + filepath.ToSlash(dir) + "/priva%74e/README"
	}
	scEnc := newScan(dir, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
	scEnc.candidate(encURL, plain)
	if !scEnc.places[privDir] {
		t.Fatalf("percent-encoded file:// URL %q did not place %q (out=%q)", encURL, privDir, scEnc.out)
	}
	scBadURL := newScan(dir, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
	scBadURL.candidate("file:///ws/%ZZ/README", plain)
	if !scBadURL.unknown {
		t.Fatalf("malformed file:// URL failed to set unknown=true")
	}

	existingDir := filepath.Join(dir, "existing")
	if err := os.MkdirAll(existingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	scReal := newScan(dir, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
	for _, p := range []string{"existing/missing/../new", existingDir + string(filepath.Separator) + "missing" + string(filepath.Separator) + ".." + string(filepath.Separator) + "new"} {
		if got, ok := scReal.real(p); ok {
			t.Fatalf("real(%q) = (%q, true), want false", p, got)
		}
	}

	// 26. Unquoted heredoc backticks bound parsable, execute-only intermediate dirs fail closed, authority-less file:/path resolves, curl cert options mask secrets, and UTF-8 BOM git config is detected.
	heredocTicks := "cat <<EOF\n" + strings.Repeat("`x`", maxShellWords) + "\nEOF"
	if parsable(heredocTicks) {
		t.Fatalf("parsable(heredoc with %d backtick substs) = true, want false", maxShellWords)
	}
	if p, isURL, bad := urlPath("file:/ws/repo/file"); p != "/ws/repo/file" || !isURL || bad {
		t.Fatalf("urlPath(file:/ws/repo/file) = (%q, %v, %v), want (\"/ws/repo/file\", true, false)", p, isURL, bad)
	}
	authlessURL := "file:" + filepath.ToSlash(privFile)
	if !strings.HasPrefix(filepath.ToSlash(privFile), "/") {
		authlessURL = "file:/" + filepath.ToSlash(privFile)
	}
	scAuthless := newScan(dir, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
	scAuthless.candidate(authlessURL, plain)
	if !scAuthless.places[privDir] {
		t.Fatalf("authority-less file: URL %q did not place %q (out=%q)", authlessURL, privDir, scAuthless.out)
	}
	for _, cmd := range []string{
		`curl --cert client.pem:SECRET https://example.com`,
		`curl -E client.pem:SECRET https://example.com`,
		`curl --proxy-cert client.pem:SECRET https://example.com`,
	} {
		if got := firstWords(cmd); strings.Contains(got, "SECRET") {
			t.Fatalf("firstWords(%q) = %q, leaked curl cert password", cmd, got)
		}
	}
	bomRepo := filepath.Join(dir, "bom-repo")
	if err := os.MkdirAll(filepath.Join(bomRepo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bomRepo, ".git", "config"), []byte("\ufeff[core] worktree = /private\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scBOM := newScan(bomRepo, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
	if got, ok := scBOM.safeLocate(bomRepo); ok {
		t.Fatalf("safeLocate(UTF-8 BOM worktree config) = (%q, true), want false", got)
	}
	if filepath.Separator == '/' && os.Geteuid() != 0 {
		execOnly := filepath.Join(dir, "exec-only")
		hidden := filepath.Join(execOnly, "hidden")
		if err := os.MkdirAll(hidden, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(hidden, "file"), []byte("ok"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(execOnly, 0o111); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(execOnly, 0o700) })
		scExec := newScan(dir, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
		scExec.candidate(filepath.Join(hidden, "file"), plain)
		if !scExec.unknown {
			t.Fatalf("traversal through execute-only dir failed to set unknown=true")
		}
		// 27. Non-ENOENT lstat errors (e.g. EACCES in a read-only non-searchable 0400 dir) fail closed instead of pretending missing.
		readOnlyDir := filepath.Join(dir, "readonly-nosearch")
		if err := os.MkdirAll(readOnlyDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(readOnlyDir, "config"), []byte("[core]\n\tworktree = /private\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(readOnlyDir, 0o400); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(readOnlyDir, 0o700) })
		scNoSearch := newScan(dir, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
		if _, err := scNoSearch.readSmallFile(filepath.Join(readOnlyDir, "config")); !errors.Is(err, errOpaque) || !scNoSearch.unknown {
			t.Fatalf("readSmallFile(0400 dir/config) = (err=%v, unknown=%v), want (errOpaque, true)", err, scNoSearch.unknown)
		}
	}
	// Unresolved config-dir env vars prevent hasGoEnv from predicting default GOPATH.
	scGoEnv := newScan(dir, func(k string) (string, bool) {
		if k == "HOME" || k == "USERPROFILE" {
			return dir, true
		}
		return "", false
	}, work{deadline: time.Now().Add(time.Minute)})
	scGoEnv.vars["AppData"] = nil
	scGoEnv.vars["XDG_CONFIG_HOME"] = nil
	scGoEnv.vars["HOME"] = nil
	if !scGoEnv.hasGoEnv("GOPATH") {
		t.Fatalf("hasGoEnv(GOPATH) = false with unresolved config-dir vars, want true")
	}
	// Whitespace-separated worktree config and GIT_CONFIG_COUNT env overrides prevent safeLocate from returning a guessed root.
	wsRepo := filepath.Join(dir, "ws-worktree-repo")
	if err := os.MkdirAll(filepath.Join(wsRepo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsRepo, ".git", "config"), []byte("[core]\n\tworktree /outside/checkout\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scWS := newScan(wsRepo, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
	if got, ok := scWS.safeLocate(wsRepo); ok {
		t.Fatalf("safeLocate(whitespace worktree config) = (%q, true), want false", got)
	}
	cleanRepo := filepath.Join(dir, "cfg-count-repo")
	if err := os.MkdirAll(filepath.Join(cleanRepo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	scCfgCount := newScan(cleanRepo, func(k string) (string, bool) {
		if k == "GIT_CONFIG_COUNT" {
			return "1", true
		}
		return "", false
	}, work{deadline: time.Now().Add(time.Minute)})
	if got, ok := scCfgCount.safeLocate(cleanRepo); ok {
		t.Fatalf("safeLocate(GIT_CONFIG_COUNT=1) = (%q, true), want false", got)
	}
}

func TestRealDotAndNestingBound(t *testing.T) {
	t.Parallel()
	if maxShellNesting > 256 {
		t.Fatalf("maxShellNesting = %d, want <= 256", maxShellNesting)
	}
	nested := strings.Repeat("$(x ", maxShellNesting+1) + strings.Repeat(")", maxShellNesting+1)
	if parsable(nested) {
		t.Fatalf("parsable(%d nested command substitutions) = true, want false", maxShellNesting+1)
	}

	dir, _ := filepath.EvalSymlinks(t.TempDir())
	existing := filepath.Join(dir, "existing")
	if err := os.MkdirAll(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(existing, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sc := newScan(dir, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
	for _, bad := range []string{"existing/missing/.", "existing/missing/./sub", "existing/file/."} {
		if got, ok := sc.real(bad); ok {
			t.Fatalf("real(%q) = (%q, true), want false", bad, got)
		}
	}
	if got, ok := sc.real("existing/./missing"); !ok || got != filepath.Join(existing, "missing") {
		t.Fatalf("real(existing/./missing) = (%q, %v), want (%q, true)", got, ok, filepath.Join(existing, "missing"))
	}
	if got, ok := sc.real("existing/."); !ok || got != existing {
		t.Fatalf("real(existing/.) = (%q, %v), want (%q, true)", got, ok, existing)
	}
	other := filepath.Join(dir, "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(other, "secret.txt")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(existing, "link")
	if err := os.Symlink(target, link); err == nil {
		scLink := newScan(existing, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
		scLink.reach("./link", existing)
		if !scLink.places[existing] || !scLink.places[other] {
			t.Fatalf("reach(./link) places = %v, want both %q and %q", scLink.places, existing, other)
		}
	}
}

func TestDirHandleIdentityCheck(t *testing.T) {
	t.Parallel()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	subA, subB := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.MkdirAll(subA, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(subB, 0o700); err != nil {
		t.Fatal(err)
	}
	fileA, fileB := filepath.Join(dir, "fa"), filepath.Join(dir, "fb")
	if err := os.WriteFile(fileA, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	dh, err := openRootDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dh.close()
	infoA, err := dh.lstat("a")
	if err != nil {
		t.Fatal(err)
	}
	infoB, err := dh.lstat("b")
	if err != nil {
		t.Fatal(err)
	}
	if sub, err := dh.openSubDir("a", infoA); err != nil {
		t.Fatalf("openSubDir(a, infoA) err = %v, want nil", err)
	} else {
		sub.close()
	}
	if sub, err := dh.openSubDir("a", infoB); !errors.Is(err, errOpaque) {
		if err == nil {
			sub.close()
		}
		t.Fatalf("openSubDir(a, infoB) err = %v, want errOpaque", err)
	}
	fiA, err := dh.lstat("fa")
	if err != nil {
		t.Fatal(err)
	}
	fiB, err := dh.lstat("fb")
	if err != nil {
		t.Fatal(err)
	}
	if f, err := dh.open("fa", false, fiA); err != nil {
		t.Fatalf("open(fa, false, fiA) err = %v, want nil", err)
	} else {
		_ = f.Close()
	}
	if f, err := dh.open("fa", false, fiB); !errors.Is(err, errOpaque) {
		if f != nil {
			_ = f.Close()
		}
		t.Fatalf("open(fa, false, fiB) err = %v, want errOpaque", err)
	}
	if f, err := dh.open("fa", true, fiA); !errors.Is(err, errOpaque) && err == nil {
		_ = f.Close()
		t.Fatalf("open(fa, true, fiA) succeeded, want error")
	}

	// Adjacent quote segments are bounded before parsing.
	if parsable("echo " + strings.Repeat(`""`, maxShellWords+1)) {
		t.Fatalf("parsable(adjacent empty double quotes) = true, want false")
	}
	if parsable("echo " + strings.Repeat(`''`, maxShellWords+1)) {
		t.Fatalf("parsable(adjacent empty single quotes) = true, want false")
	}

	// Unset HOME stays unset for $HOME parameter expansion while tilde path() falls back to UserHomeDir.
	scNoHome := newScan(dir, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(time.Minute)})
	if scNoHome.Get("HOME").IsSet() {
		t.Fatalf("Get(HOME).IsSet() = true with empty env, want false")
	}
	fHome, err := syntax.NewParser().Parse(strings.NewReader(`echo "$HOME/repo/file"`), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scNoHome.fields(fHome.Stmts[0].Cmd.(*syntax.CallExpr).Args[1]); !errors.Is(err, errOpaque) {
		t.Fatalf(`fields("$HOME/repo/file") with unset HOME err = %v, want errOpaque`, err)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if got, ok := scNoHome.path("~/repo/file", dir); !ok || got != filepath.Join(home, "repo", "file") {
			t.Fatalf("path(~/repo/file) with unset HOME = (%q, %v), want (%q, true)", got, ok, filepath.Join(home, "repo", "file"))
		}
	}
}

func TestGitfilePrefixCommentsOverlongAndResolveMayCreate(t *testing.T) {
	t.Parallel()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	realGit := filepath.Join(root, "real.git")
	if err := os.MkdirAll(realGit, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		content string
		wantOK  bool
	}{
		{"valid-lf", "gitdir: " + realGit + "\n", true},
		{"valid-crlf", "gitdir: " + realGit + "\r\n", true},
		{"missing-space", "gitdir:" + realGit + "\n", false},
		{"leading-space", " gitdir: " + realGit + "\n", false},
		{"extra-newline", "gitdir: " + realGit + "\n\n", false},
	} {
		wt := filepath.Join(root, tc.name)
		if err := os.MkdirAll(wt, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, ".git"), []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
		sc := newScan(wt, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
		got, ok := sc.safeLocate(wt)
		if ok != tc.wantOK || (tc.wantOK && got != wt) {
			t.Errorf("safeLocate(%s) = (%q, %v), want ok=%v", tc.name, got, ok, tc.wantOK)
		}
	}

	// Comment lines are bounded before invoking the shell parser.
	if parsable(strings.Repeat("#\n", maxShellWords/2+1)) {
		t.Fatalf("parsable(repeated #\\n) = true, want false")
	}

	// Overlong non-slashed plain words do not fail closed; overlong slashed plain paths and targets do.
	side := filepath.Join(root, "side")
	api := filepath.Join(root, "acme-api")
	if err := os.MkdirAll(side, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(api, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(api, "NOTES.md"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	scLong := newScan(side, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	scLong.candidate(strings.Repeat("a", 200<<10), plain)
	scLong.candidate(strings.Repeat("b", 300), plain)
	if scLong.unknown {
		t.Fatalf("overlong non-slashed plain candidates set unknown=true")
	}
	scLong.candidate("/"+strings.Repeat("a/", maxPathBytes/2+2), plain)
	if !scLong.unknown {
		t.Fatalf("overlong slashed plain candidate did not fail closed")
	}

	// resolve does not fall back to mayCreate on sc.cwd when sc.dir already matched an existing file.
	scDir := newScan(side, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	scDir.dir = root
	scDir.candidate("acme-api/NOTES.md", plain)
	if !slices.Equal(scDir.out, []string{api}) {
		t.Fatalf("out after dir-matched relative file = %q, want [%q]", scDir.out, api)
	}
}

func TestAbbreviatedSecretOptionsAndLogicalCd(t *testing.T) {
	t.Parallel()
	// Curl-style abbreviated long secret options mask their following value, while non-secret flags do not.
	for _, cmd := range []string{
		"curl --oauth2-b SECRET https://example.com",
		"curl --proxy-u alice:SECRET https://example.com",
		"curl --proxy-h 'Authorization: SECRET' https://example.com",
		"curl --data-u token=SECRET https://example.com",
	} {
		if got := firstWords(cmd); strings.Contains(got, "SECRET") {
			t.Errorf("firstWords(%q) = %q, leaked SECRET", cmd, got)
		}
	}
	if got := firstWords("curl --config app.cfg --help"); !strings.Contains(got, "app.cfg") {
		t.Errorf("firstWords(benign --config) = %q, unexpectedly masked app.cfg", got)
	}

	// Default target (cd -L) normalizes .. logically across symlinks; plain operands and physTarget (cd -P) stay physical.
	root, _ := filepath.EvalSymlinks(t.TempDir())
	logicalBase := filepath.Join(root, "logical")
	logicalSibling := filepath.Join(logicalBase, "sibling")
	outside := filepath.Join(root, "outside")
	outsideSub := filepath.Join(outside, "subdir")
	for _, d := range []string{logicalSibling, outsideSub} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(logicalBase, "NOTES.md"), []byte("logical"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "OUTSIDE.md"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(logicalBase, "link")
	if err := os.Symlink(outsideSub, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}

	sep := string(filepath.Separator)
	// 1. cd /logical/link/.. enters /logical logically, so subsequent NOTES.md reaches /logical.
	sc1 := newScan(root, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	sc1.candidate(link+sep+"..", target)
	if sc1.dir != logicalBase {
		t.Fatalf("target(link/..).dir = %q, want %q", sc1.dir, logicalBase)
	}
	sc1.candidate("NOTES.md", plain)
	if !slices.Contains(sc1.out, logicalBase) || !slices.Contains(sc1.out, outsideSub) {
		t.Fatalf("target(link/..) out = %q, want both %q and %q", sc1.out, logicalBase, outsideSub)
	}

	// 2. cd /logical/link/../sibling succeeds logically even though /outside/sibling does not exist.
	sc2 := newScan(root, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	sc2.candidate(link+sep+".."+sep+"sibling", target)
	if sc2.dir != logicalSibling || !slices.Contains(sc2.out, logicalSibling) {
		t.Fatalf("target(link/../sibling) dir=%q out=%q, want %q", sc2.dir, sc2.out, logicalSibling)
	}

	// 3. cd /logical/link preserves logical dir; plain ../OUTSIDE.md traverses physically, while target .. goes logically to /logical.
	sc3 := newScan(root, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	sc3.candidate(link, target)
	if sc3.dir != link {
		t.Fatalf("target(link).dir = %q, want %q", sc3.dir, link)
	}
	sc3.candidate("../OUTSIDE.md", plain)
	if !slices.Contains(sc3.out, outside) {
		t.Fatalf("plain(../OUTSIDE.md) from logical symlink dir out = %q, want %q", sc3.out, outside)
	}
	sc3.candidate("..", target)
	if sc3.dir != logicalBase {
		t.Fatalf("target(..) after target(link) dir = %q, want %q", sc3.dir, logicalBase)
	}

	// 4. physTarget (cd -P) resolves symlinks physically for both link/.. and link && cd ...
	sc4 := newScan(root, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	sc4.candidate(link+sep+"..", physTarget)
	if sc4.dir != outside {
		t.Fatalf("physTarget(link/..).dir = %q, want %q", sc4.dir, outside)
	}
	sc5 := newScan(root, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	sc5.candidate(link, physTarget)
	if sc5.dir != outsideSub {
		t.Fatalf("physTarget(link).dir = %q, want %q", sc5.dir, outsideSub)
	}
	sc5.candidate("..", target)
	if sc5.dir != outside {
		t.Fatalf("target(..) after physTarget(link) dir = %q, want %q", sc5.dir, outside)
	}

	// 5. Missing or regular-file component before .. fails logical cd without mutating sc.dir.
	sc6 := newScan(root, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	sc6.candidate(filepath.Join(logicalBase, "missing")+sep+"..", target)
	sc6.candidate(filepath.Join(logicalBase, "NOTES.md")+sep+"..", target)
	if sc6.dir != root {
		t.Fatalf("invalid logical target mutated dir to %q, want %q", sc6.dir, root)
	}

	// 6. Logical cd .. from a normal (non-symlink) directory does not place the starting directory.
	sc7 := newScan(logicalSibling, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	sc7.candidate(".."+sep+"missing", target)
	if len(sc7.out) != 0 {
		t.Fatalf("failed cd ../missing from non-symlink dir placed %q, want empty", sc7.out)
	}

	// 7. safeLocate fails closed on GIT_CONFIG_GLOBAL/SYSTEM/GIT_CONFIG and on ~/.gitconfig core.worktree.
	gitRepo := filepath.Join(root, "git-repo")
	if err := os.MkdirAll(filepath.Join(gitRepo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	fakeHome := filepath.Join(root, "fake-home")
	if err := os.MkdirAll(fakeHome, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, envKey := range []string{"GIT_CONFIG", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM"} {
		scEnv := newScan(gitRepo, func(k string) (string, bool) {
			if k == envKey {
				return filepath.Join(fakeHome, "alt.gitconfig"), true
			}
			if k == "HOME" {
				return fakeHome, true
			}
			return "", false
		}, work{deadline: time.Now().Add(time.Minute)})
		if got, ok := scEnv.safeLocate(gitRepo); ok {
			t.Fatalf("safeLocate(%s set) = (%q, true), want false", envKey, got)
		}
	}
	if err := os.WriteFile(filepath.Join(fakeHome, ".gitconfig"), []byte("[core]\n\tworktree = /outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scGlobal := newScan(gitRepo, func(k string) (string, bool) {
		if k == "HOME" {
			return fakeHome, true
		}
		return "", false
	}, work{deadline: time.Now().Add(time.Minute)})
	if got, ok := scGlobal.safeLocate(gitRepo); ok {
		t.Fatalf("safeLocate(~/.gitconfig worktree) = (%q, true), want false", got)
	}

	// 8. Eval-script options (-e, --eval, --ev, -pe) mask their script argument in firstWords.
	for _, cmd := range []string{
		`node -e 'console.log("SECRET")' /missing/path`,
		`node --eval 'console.log("SECRET")' /missing/path`,
		`node --ev 'SECRET' /missing/path`,
		`node -pe 'SECRET'`,
		`perl -e 'print "SECRET"'`,
	} {
		if got := firstWords(cmd); strings.Contains(got, "SECRET") {
			t.Errorf("firstWords(%q) = %q, leaked SECRET", cmd, got)
		}
	}

	// 9. parsable fast-skips 1 MiB whitespace payloads within the hook latency budget.
	for _, ws := range []string{strings.Repeat(" ", maxShellBytes), strings.Repeat("\n", maxShellBytes), strings.Repeat(" \t\n", maxShellBytes/3)} {
		if !parsable(ws) {
			t.Fatalf("parsable(1MiB whitespace) = false, want true")
		}
	}
}

func FuzzParsable(f *testing.F) {
	for _, seed := range []string{
		"ls -la",
		"cat <<'EOF'\nhello\nEOF\nls",
		"cat <<-EOF\n\tEOF",
		"echo $((1+2)) ${a:-b} $(pwd)",
		`echo "a\"b" 'c' $'d\'e'`,
		"if true; then echo hi; fi",
		"let x=1<<2",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, src string) {
		if len(src) > 4096 {
			return
		}
		_ = parsable(src)
	})
}
