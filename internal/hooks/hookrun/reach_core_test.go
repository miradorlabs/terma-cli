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
		"redis-cli -a mypass ping":                                                       "redis-cli -a … ping",
		"ENV=x redis-cli -a mypass":                                                      "ENV=… redis-cli -a …",
		"sudo redis-cli -a mypass":                                                       "sudo redis-cli -a …",
		`echo don\'t --password mypass`:                                                  `echo don\'t --password …`,
		"curl -d secretbody https://example.com":                                         "curl -d … https://example.com",
		"curl --oauth2-bearer tok https://example.com":                                   "curl --oauth2-bearer … https://example.com",
		"cli --api-key supersecret run":                                                  "cli --api-key … run",
		`cli "--password" hunter2 run`:                                                   `cli "--password" … run`,
		"curl https://u:p@ss@example.com/a":                                              "curl https://…@example.com/a",
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
	for _, tc := range []struct {
		in    string
		path  string
		isURL bool
	}{
		{"https://example.com/a/b", "", true},
		{"git+ssh://github.com/acme/api", "", true},
		{"file:///tmp/acme-api/NOTES.md", "/tmp/acme-api/NOTES.md", true},
		{"file:///C:/work/api", "C:/work/api", true},
		{"file://server/share/api", "//server/share/api", true},
		{"../acme-api/NOTES.md", "", false},
		{"C:/work/api", "", false},
	} {
		p, ok := urlPath(tc.in)
		if p != tc.path || ok != tc.isURL {
			t.Errorf("urlPath(%q) = (%q, %v), want (%q, %v)", tc.in, p, ok, tc.path, tc.isURL)
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
		case "HOME":
			return parent, true
		case "GOPATH":
			return filepath.Join(parent, "go"), true
		}
		return os.LookupEnv(k)
	}
	sc := newScan(side, env, work{deadline: time.Now().Add(time.Minute)})
	evalSubst := func(src string) (string, error) {
		f, err := syntax.NewParser().Parse(strings.NewReader("echo $("+src+")"), "")
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		ce := f.Stmts[0].Cmd.(*syntax.CallExpr)
		cs := ce.Args[1].Parts[0].(*syntax.CmdSubst)
		var buf bytes.Buffer
		err = sc.substitute(&buf, cs)
		return buf.String(), err
	}
	for src, want := range map[string]string{
		"pwd":                           side,
		"git rev-parse --show-toplevel": side,
		"go env GOPATH":                 filepath.Join(parent, "go"),
		"go env GOMODCACHE":             filepath.Join(parent, "go", "pkg", "mod"),
		"dirname /a/b/c":                "/a/b",
		"basename /a/b/c":               "c",
		"realpath .":                    side,
		"readlink -f .":                 side,
		"pwd -P":                        side,
		"dirname $(pwd)":                parent,
	} {
		got, err := evalSubst(src)
		if err != nil || got != want {
			t.Errorf("substitute(%q) = (%q, %v), want (%q, nil)", src, got, err, want)
		}
	}
	if got, err := evalSubst("mktemp -d"); err != nil || !strings.HasPrefix(got, os.TempDir()) {
		t.Errorf("substitute(mktemp -d) = (%q, %v)", got, err)
	}
	if got, err := evalSubst("mktemp -dt foo.XXX"); err != nil || !strings.HasPrefix(got, os.TempDir()) {
		t.Errorf("substitute(mktemp -dt) = (%q, %v)", got, err)
	}
	for _, opaque := range []string{"cat somefile", "mktemp -p /tmp", "mktemp -dp/tmp", "mktemp --tmpdir=/tmp", "mktemp sub/foo.XXX"} {
		if _, err := evalSubst(opaque); err == nil {
			t.Errorf("substitute(%q) succeeded, want error", opaque)
		}
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
	// Moderately deep nonexistent path spends lookup budget per parent step.
	deep := strings.Repeat("a/", 50) + "leaf"
	sc2 := newScan(dir, os.LookupEnv, work{deadline: time.Now().Add(time.Minute)})
	sc2.candidate(deep, plain)
	if sc2.lookups < 50 {
		t.Fatalf("deep reachNew spent only %d lookups, want >= 50", sc2.lookups)
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
	hugeGP := strings.Repeat("x ", 250_000)
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
