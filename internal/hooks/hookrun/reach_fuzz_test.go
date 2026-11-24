package hookrun

import (
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"

	"mvdan.cc/sh/v3/syntax"
)

// Bounds on what ShellPaths may take for any command, which its own bounds keep it within:
// the hook's budget of time, ten times as long under the race detector, which slows a scan
// some tenfold, and fuzzAlloc bytes allocated.
const (
	fuzzTime  = 50 * time.Millisecond
	fuzzAlloc = 64 << 20
)

// Any command, however malformed, ShellPaths reads without panicking, within the hook's
// budget of time and fuzzAlloc. The seeds are the corpus's commands, crossing and not, and
// each of them run as timeout 60 bash -c's script. What a command that parses reaches, it
// reaches run in a subshell, after true &&, after ls .. &&, and as the script of a shell
// behind a wrapper not known (direnv exec . bash -c): each of its places, or somewhere not
// known. So it does read as on Windows too (shellReadings), on any system, where that
// reading parses, but behind a shell: the script is quoted for a POSIX shell, which alone
// reads it back as the command, and the Windows reading takes the quoting's backslashes for
// separators, reading another command than the command's own Windows reading. There the
// command's places are those a POSIX shell's reading of it reaches.
func FuzzShellPaths(f *testing.F) {
	c := newCorpus(f)
	for _, set := range []map[string]string{corpusFine(), corpusCrossing(c)} {
		for _, command := range set {
			f.Add(command)
			f.Add(underTimeout(command))
		}
	}
	limit := fuzzTime
	if info, ok := debug.ReadBuildInfo(); ok && slices.Contains(info.Settings, debug.BuildSetting{Key: "-race", Value: "true"}) {
		limit *= 10
	}
	// A hook's deadline, with panic recovery disabled so panics fail the fuzz test.
	paths := func(command string, windows bool) []string {
		return readShellWith(command, shellReadings(command, windows), c.side, c.env, work{
			deadline:  time.Now().Add(shellTime),
			noRecover: true,
		})
	}
	f.Fuzz(func(t *testing.T, command string) {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()
		paths(command, runtime.GOOS == "windows")
		took := time.Since(start)
		runtime.ReadMemStats(&after)
		// Retry on contention (parallel workers or GC); check wall time generously on seed runs.
		for retries := 2; took > limit && retries > 0; retries-- {
			start = time.Now()
			paths(command, runtime.GOOS == "windows")
			took = min(took, time.Since(start))
		}
		if took > 4*limit {
			t.Errorf("ShellPaths(%.80q…) took %v, past %v", command, took, 4*limit)
		}
		if alloc := after.TotalAlloc - before.TotalAlloc; alloc > fuzzAlloc {
			t.Errorf("ShellPaths(%.80q…) allocated %d MiB, past %d", command, alloc>>20, fuzzAlloc>>20)
		}
		if !parses(command) {
			return
		}
		posix := paths(command, false)
		for _, windows := range []bool{false, true} {
			places := posix
			if windows {
				if !strings.ContainsRune(command, '\\') || !parses(windowsReading(command)) {
					continue // read alike, or up to an error, which a wrapper moves
				}
				places = paths(command, true)
			}
			for i, variant := range []string{
				"(\n" + command + "\n)",
				"true && " + command,
				"ls .. && " + command,
				"direnv exec . bash -c " + quote(command),
			} {
				// A command ending in a backslash runs on past the newline, into the subshell's close.
				if i == 0 && (len(command)-len(strings.TrimRight(command, `\`)))%2 == 1 || !parses(variant) {
					continue
				}
				got := paths(variant, windows)
				if slices.ContainsFunc(got, isUnknownReach) {
					continue
				}
				want := places
				if i == 3 {
					want = posix // quoted for a POSIX shell
				}
				for _, p := range want {
					if !isUnknownReach(p) && !slices.Contains(got, p) {
						t.Errorf("ShellPaths(%.80q…), Windows reading %v, reaches %q, but not as %.120q: %q", command, windows, p, variant, got)
					}
				}
			}
		}
	})
}

// parses reports whether command parses as bash, within the parser's bounds.
func parses(command string) bool {
	if len(command) > maxShellBytes || !parsable(command) {
		return false
	}
	_, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	return err == nil
}

// quote is s in single quotes, as a shell reads it back.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
