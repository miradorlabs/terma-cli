package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// The version of the Antigravity binary running a hook rides on the session's start, its
// observations and its tool calls as terma.main_agent.version. A hook's payload and
// environment name neither, but the process that starts a hook is that binary, through one
// shell: agy for the CLI, or the copy T3 Code bundles, run as `antigravity acp`. Asking it
// takes 70 to 90 ms, so each binary's answer is kept under the state directory, keyed by
// the binary's path, size and modification time.

// antigravityVersionDir keeps one entry per Antigravity binary file, named by the hash of
// its path, size and modification time. An update is another file, so it has an entry of its
// own, and a session that outlives the update keeps finding the old one. The relay's sweep
// ages entries out like other hook state (hookrun.Sweep), and a binary whose entry went is
// asked once more.
var antigravityVersionDir = hookrun.AgentStateDir(Agent{}.Name(), "versions")

const (
	// antigravityVersionDepth is how far up a hook's ancestors the binary is looked for: the
	// hook's shell is the first, the binary the second.
	antigravityVersionDepth = 4
	// antigravityVersionLimit bounds asking a binary; one that has not answered by then is
	// stopped and kept as not saying.
	antigravityVersionLimit = 2 * time.Second
	// antigravityVersionRetry is how long "would not say" is kept before the binary is asked
	// again: a hang costs its limit once in that time, not at every hook, and a first run
	// that was only slow is not held against the binary for good.
	antigravityVersionRetry = time.Hour
	// antigravityVersionOutput is as much of the answer as is read.
	antigravityVersionOutput = 4 << 10
)

// antigravityBinaryNames are the names an Antigravity binary runs as. Only a file of one of
// them is run.
var antigravityBinaryNames = map[string]bool{"agy": true, "antigravity": true}

// The lookup's seams, which tests replace. TestMain makes the ancestors none, so no test
// runs a binary unless it sets them.
var (
	versionAncestors  = procinfo.Ancestors
	versionExecutable = procinfo.ExecutableOf
	askVersion        = askBinaryVersion
)

// antigravityVersionEntry is one binary's kept answer.
type antigravityVersionEntry struct {
	// Path is the binary's path, which the entry's name hashes.
	Path string `json:"path"`
	// Size and ModTime are the file's when it answered, in bytes and Unix nanoseconds.
	Size    int64 `json:"size"`
	ModTime int64 `json:"mod_time"`
	// Version is its answer, reduced to the number; "" when it would not say.
	Version string `json:"version"`
}

// lazyAntigravityVersion is the running binary's version, looked up on the first call and
// kept for the hook's next.
func lazyAntigravityVersion(ctx context.Context, env hookrun.Env) func() string {
	return sync.OnceValue(func() string { return antigravityVersion(ctx, env.StateDir) })
}

// withAgentVersion adds the running binary's version to a when it is known.
func withAgentVersion(a map[string]any, version func() string) map[string]any {
	if version != nil {
		if v := version(); v != "" {
			a[semconv.TermaMainAgentVersionKey] = v
		}
	}
	return a
}

// antigravityVersion is the version of the Antigravity binary running this hook, "" when it
// cannot tell: no such binary among the hook's nearest ancestors, a system that cannot say
// what a process runs (Windows), or a binary that will not say. Never the agy on PATH.
func antigravityVersion(ctx context.Context, stateDir string) string {
	exe, ok := runningAntigravity()
	if !ok {
		return ""
	}
	info, err := os.Stat(exe.Run)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	entry := antigravityVersionEntry{Path: exe.Path, Size: info.Size(), ModTime: info.ModTime().UnixNano()}
	path := ""
	if stateDir != "" {
		key := fmt.Sprintf("%s|%d|%d", entry.Path, entry.Size, entry.ModTime)
		path = filepath.Join(stateDir, antigravityVersionDir, hookrun.EvidenceID(key)+".json")
	}
	if kept, ok := readAntigravityVersion(path); ok && kept.Path == entry.Path && kept.Size == entry.Size && kept.ModTime == entry.ModTime {
		return kept.Version
	}
	out, asked := askVersion(ctx, exe)
	if !asked {
		return ""
	}
	entry.Version = agyVersionRE.FindString(out)
	writeAntigravityVersion(path, entry)
	return entry.Version
}

// runningAntigravity is the nearest of the hook's first ancestors whose file is named as an
// Antigravity binary.
func runningAntigravity() (procinfo.Executable, bool) {
	ancestors := versionAncestors()
	if len(ancestors) > antigravityVersionDepth {
		ancestors = ancestors[:antigravityVersionDepth]
	}
	for _, pid := range ancestors {
		if exe, ok := versionExecutable(pid); ok && antigravityBinaryNames[filepath.Base(exe.Path)] {
			return exe, true
		}
	}
	return procinfo.Executable{}, false
}

// readAntigravityVersion is the kept entry at path, if it reads and holds a version terma
// would have kept. One that holds none stands for antigravityVersionRetry, by the file's age.
func readAntigravityVersion(path string) (antigravityVersionEntry, bool) {
	var e antigravityVersionEntry
	if path == "" {
		return e, false
	}
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &e) != nil || agyVersionRE.FindString(e.Version) != e.Version {
		return antigravityVersionEntry{}, false
	}
	if e.Version == "" {
		if info, err := os.Stat(path); err != nil || time.Since(info.ModTime()) > antigravityVersionRetry {
			return antigravityVersionEntry{}, false
		}
	}
	return e, true
}

// writeAntigravityVersion keeps e at path, through a temporary file and a rename. A failure
// only means the binary is asked again at the next hook.
func writeAntigravityVersion(path string, e antigravityVersionEntry) {
	if path == "" {
		return
	}
	b, err := json.Marshal(e)
	if err != nil || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	_ = hookrun.WriteState(path, b)
}

// askBinaryVersion runs exe with --version, no input and a deadline, and returns what it
// printed and whether it was asked: a binary that started is answered for, even by its own
// deadline or an exit status. One the hook's own deadline cut short was not asked, so
// nothing is kept against it. It runs under the name it started as, also when it is reached
// through another path (procinfo.Executable).
func askBinaryVersion(parent context.Context, exe procinfo.Executable) (string, bool) {
	ctx, cancel := context.WithTimeout(parent, antigravityVersionLimit)
	defer cancel()
	var out cappedBuffer
	cmd := exec.CommandContext(ctx, exe.Run, "--version")
	cmd.Args[0] = exe.Path
	cmd.Stdout = &out
	cmd.WaitDelay = 100 * time.Millisecond
	if err := cmd.Start(); err != nil {
		return "", false
	}
	_ = cmd.Wait()
	if parent.Err() != nil {
		return "", false
	}
	return out.String(), true
}

// cappedBuffer keeps the first antigravityVersionOutput bytes written to it.
type cappedBuffer struct{ bytes.Buffer }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := antigravityVersionOutput - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
