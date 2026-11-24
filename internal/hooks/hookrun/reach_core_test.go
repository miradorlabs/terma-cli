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
	if got, ok := whereP(scPwd, nil); !ok || got != resolvedRealSub {
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
		if got, ok := scRealLeaf.real(dangling); ok {
			t.Fatalf("real(dangling) = (%q, true), want false", got)
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

	// 9. firstWords masks command/process substitutions (both $(...) and backticks) after secret flags, in arguments, and in redirections.
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
