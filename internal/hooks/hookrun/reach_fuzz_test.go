package hookrun

import (
	"runtime"
	"runtime/debug"
	"slices"
	"testing"
	"time"
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
// each of them run as timeout 60 bash -c's script; testdata/fuzz holds the inputs a run
// found past the bounds, since fixed (a word of 4000 braces, maxShellBraces).
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
	f.Fuzz(func(t *testing.T, command string) {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()
		c.paths(command)
		took := time.Since(start)
		runtime.ReadMemStats(&after)
		// A run past the limit may have waited on another's (the fuzzer runs a worker on
		// each core) or on the collector: the fastest of three is the command's own.
		for retries := 2; took > limit && retries > 0; retries-- {
			start = time.Now()
			c.paths(command)
			took = min(took, time.Since(start))
		}
		if took > limit {
			t.Errorf("ShellPaths(%.80q…) took %v, past %v", command, took, limit)
		}
		if alloc := after.TotalAlloc - before.TotalAlloc; alloc > fuzzAlloc {
			t.Errorf("ShellPaths(%.80q…) allocated %d MiB, past %d", command, alloc>>20, fuzzAlloc>>20)
		}
	})
}
