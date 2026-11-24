package antigravity

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// These tests replace the package's version seams, so none of them runs in parallel.

// versionSeams is a made-up process tree: the hook's ancestors, each running a file, and a
// binary that answers --version, with what was asked counted.
type versionSeams struct {
	t *testing.T
	// answer is what every binary prints; asked is every file it was asked of.
	answer string
	asked  []string
	// lookups counts the walks of the ancestors.
	lookups int
}

// fakeVersions makes the hook's ancestors pids 100, 101 and on, running exes in order.
func fakeVersions(t *testing.T, answer string, exes ...procinfo.Executable) *versionSeams {
	t.Helper()
	s := &versionSeams{t: t, answer: answer}
	ancestors, executable, ask := versionAncestors, versionExecutable, askVersion
	t.Cleanup(func() { versionAncestors, versionExecutable, askVersion = ancestors, executable, ask })
	byPID := map[int]procinfo.Executable{}
	var pids []int
	for i, exe := range exes {
		byPID[100+i] = exe
		pids = append(pids, 100+i)
	}
	versionAncestors = func() []int { s.lookups++; return pids }
	versionExecutable = func(pid int) (procinfo.Executable, bool) { exe, ok := byPID[pid]; return exe, ok }
	askVersion = func(_ context.Context, exe procinfo.Executable) (string, bool) {
		s.asked = append(s.asked, exe.Run)
		return s.answer, true
	}
	return s
}

// binary is a made-up file named name, of size bytes, run from its own path.
func binary(t *testing.T, name string, size int) procinfo.Executable {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, make([]byte, size), 0o700); err != nil {
		t.Fatal(err)
	}
	return procinfo.Executable{Path: path, Run: path}
}

func TestAntigravityVersionIsTheNearestAntigravityBinary(t *testing.T) {
	sh, other := binary(t, "sh", 1), binary(t, "node", 1)
	t3, cli := binary(t, "antigravity", 2), binary(t, "agy", 3)
	// A binary replaced since it started: its old path, reached through another file.
	replaced := procinfo.Executable{Path: "/made-up/old/agy", Run: binary(t, "running", 4).Run}
	for _, tc := range []struct {
		name  string
		chain []procinfo.Executable
		want  string
	}{
		{"the nearest wins", []procinfo.Executable{sh, t3, cli}, t3.Run},
		{"agy", []procinfo.Executable{sh, cli, t3}, cli.Run},
		{"neither", []procinfo.Executable{sh, other, sh}, ""},
		{"past four", []procinfo.Executable{sh, sh, other, sh, cli}, ""},
		{"fourth", []procinfo.Executable{sh, sh, other, cli}, cli.Run},
		{"replaced", []procinfo.Executable{sh, replaced}, replaced.Run},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fakeVersions(t, "1.5.14\n", tc.chain...)
			got := antigravityVersion(context.Background(), t.TempDir())
			if tc.want == "" {
				if got != "" || len(s.asked) != 0 {
					t.Fatalf("version %q, asked %v, from a chain with no Antigravity binary", got, s.asked)
				}
				return
			}
			if got != "1.5.14" || len(s.asked) != 1 || s.asked[0] != tc.want {
				t.Fatalf("version %q, asked %v, want 1.5.14 from %s", got, s.asked, tc.want)
			}
		})
	}
}

func TestAntigravityVersionIsKeptPerBinary(t *testing.T) {
	ctx, stateDir := context.Background(), t.TempDir()
	cli := binary(t, "agy", 3)
	s := fakeVersions(t, "1.5.14\n", cli)
	dir := filepath.Join(stateDir, antigravityVersionDir)
	ask := func(want string, asks int) {
		t.Helper()
		if got := antigravityVersion(ctx, stateDir); got != want || len(s.asked) != asks {
			t.Fatalf("version %q after %d asks, want %q after %d", got, len(s.asked), want, asks)
		}
		// One entry per file asked: an update is another file, and leaves the old one's entry.
		if entries, _ := os.ReadDir(dir); len(entries) != asks {
			t.Fatalf("the folder holds %d entries, want %d", len(entries), asks)
		}
	}
	ask("1.5.14", 1) // a miss asks and keeps the answer
	ask("1.5.14", 1) // a hit asks nothing
	if err := os.WriteFile(cli.Run, make([]byte, 5), 0o700); err != nil {
		t.Fatal(err)
	}
	s.answer = "1.5.15"
	ask("1.5.15", 2) // a new size asks again, into an entry of its own
	ask("1.5.15", 2)
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(cli.Run, later, later); err != nil {
		t.Fatal(err)
	}
	s.answer = "made-up: no version here"
	ask("", 3) // a new modification time asks again, and an answer with no version is kept
	ask("", 3)
}

// "Would not say" is kept for antigravityVersionRetry, then the binary is asked again.
func TestAntigravityVersionAsksAgainAfterNoAnswer(t *testing.T) {
	ctx, stateDir := context.Background(), t.TempDir()
	s := fakeVersions(t, "", binary(t, "agy", 3))
	dir := filepath.Join(stateDir, antigravityVersionDir)
	for range 2 {
		if got := antigravityVersion(ctx, stateDir); got != "" || len(s.asked) != 1 {
			t.Fatalf("version %q after %d asks, want none after one", got, len(s.asked))
		}
	}
	old := time.Now().Add(-antigravityVersionRetry - time.Minute)
	if err := os.Chtimes(filepath.Join(dir, readOne(t, dir)), old, old); err != nil {
		t.Fatal(err)
	}
	s.answer = "1.5.14"
	for range 2 {
		if got := antigravityVersion(ctx, stateDir); got != "1.5.14" || len(s.asked) != 2 {
			t.Fatalf("version %q after %d asks, want 1.5.14 after two", got, len(s.asked))
		}
	}
}

// A binary the hook's own deadline cut short was not asked: nothing is kept against it, and
// the next hook asks. One that outlasts its own limit is answered for.
func TestAskBinaryVersionDoesNotAnswerForTheHooksDeadline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a shell script stands in for the binary")
	}
	path := filepath.Join(t.TempDir(), "agy")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 0.3\necho 1.5.14\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	exe := procinfo.Executable{Path: path, Run: path}
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if out, asked := askBinaryVersion(short, exe); asked || out != "" {
		t.Fatalf("answered %q, asked %v, for a hook out of time", out, asked)
	}
	if out, asked := askBinaryVersion(context.Background(), exe); !asked || agyVersionRE.FindString(out) != "1.5.14" {
		t.Fatalf("answered %q, asked %v, with time to spare", out, asked)
	}
}

func TestAntigravityVersionSurvivesAnUnwritableState(t *testing.T) {
	stateDir := t.TempDir()
	// The state directory's agents folder is a file: nothing can be kept under it.
	if err := os.WriteFile(filepath.Join(stateDir, "agents"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := fakeVersions(t, "1.5.14", binary(t, "agy", 1))
	for range 2 {
		if got := antigravityVersion(context.Background(), stateDir); got != "1.5.14" {
			t.Fatalf("version %q with no place to keep it", got)
		}
	}
	if len(s.asked) != 2 {
		t.Fatalf("asked %d times, want each hook to ask with nothing kept", len(s.asked))
	}
}

// The session's start, every observation and every tool call carry the version; no other
// event does, nor the start that links a subagent.
func TestAntigravityEventsCarryTheRunningVersion(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(map[bool]string{true: "known", false: "unknown"}[known], func(t *testing.T) {
			chain := []procinfo.Executable{binary(t, "sh", 1), binary(t, "agy", 1)}
			if !known {
				chain[1] = binary(t, "node", 1)
			}
			s := fakeVersions(t, "1.5.14\n", chain...)
			g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
			g.turnOne()
			events := g.drain()
			p := newTranscriptRig(t, parentConversation, "transcript_subagent_parent.jsonl")
			p.stateDir = g.stateDir // one machine: the answer kept by the first hook serves both
			p.write(1)
			p.hook(preInvocation, begin("1"))
			p.write(3)
			p.hook(postInvocation, invoked)
			for name, es := range p.drain() {
				events[name] = append(events[name], es...)
			}
			carries := map[string]bool{semconv.TermaSessionStartEvent: true, semconv.TermaSessionObservationEvent: true, semconv.TermaToolCallEvent: true}
			for _, name := range []string{semconv.TermaSessionStartEvent, semconv.TermaSessionObservationEvent, semconv.TermaToolCallEvent, semconv.TermaModelCallEvent, semconv.TermaFilesTouchedEvent} {
				if len(events[name]) == 0 {
					t.Errorf("no %s to judge", name)
				}
			}
			for name, es := range events {
				for _, e := range es {
					v, has := e.Attrs[semconv.TermaMainAgentVersionKey]
					want := known && carries[name] && e.SessionID != childConversation
					if has != want || (has && v != "1.5.14") {
						t.Errorf("%s of %s carries version %v (%v), want %v", name, e.SessionID, v, has, want)
					}
				}
			}
			if !hasSession(events[semconv.TermaSessionStartEvent], childConversation) {
				t.Error("the subagent's link did not go")
			}
			// One lookup per hook that sends: turnOne's five and the parent's two.
			if s.lookups != 7 {
				t.Errorf("%d lookups, want one per hook", s.lookups)
			}
			if known && len(s.asked) != 1 {
				t.Errorf("asked %d times, want once for every hook", len(s.asked))
			}
		})
	}
}

// A hook where nothing is collected looks up nothing.
func TestAntigravityUncollectedHookAsksNoVersion(t *testing.T) {
	s := fakeVersions(t, "1.5.14", binary(t, "sh", 1), binary(t, "agy", 1))
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(7)
	outside := t.TempDir()
	for _, h := range []func(context.Context, hookrun.Env) error{preInvocation, postToolUse, postInvocation, stop} {
		g.hookAt(h, outside, toolUse(2, "write_file", `{}`)+","+begin("1"))
	}
	g.admitted = nil
	for _, h := range []func(context.Context, hookrun.Env) error{preInvocation, postToolUse, postInvocation, stop} {
		g.hook(h, toolUse(2, "write_file", `{}`)+","+begin("1"))
	}
	if s.lookups != 0 || len(s.asked) != 0 || len(g.drain()) != 0 {
		t.Fatalf("%d lookups and %d asks where nothing is collected", s.lookups, len(s.asked))
	}
}

func hasSession(events []spool.Event, id string) bool {
	for _, e := range events {
		if e.SessionID == id {
			return true
		}
	}
	return false
}

func readOne(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries %v: %v", entries, err)
	}
	return entries[0].Name()
}

// BenchmarkAntigravityVersionKept is a hook's cost once its binary's answer is kept: the
// real walk of this process's ancestors and their files, the nearest taken as agy.
func BenchmarkAntigravityVersionKept(b *testing.B) {
	ancestors, executable, ask := versionAncestors, versionExecutable, askVersion
	b.Cleanup(func() { versionAncestors, versionExecutable, askVersion = ancestors, executable, ask })
	versionAncestors = procinfo.Ancestors
	parent := os.Getppid()
	versionExecutable = func(pid int) (procinfo.Executable, bool) {
		exe, ok := procinfo.ExecutableOf(pid)
		if ok && pid == parent {
			exe.Path = filepath.Join(filepath.Dir(exe.Path), "agy")
		}
		return exe, ok
	}
	askVersion = func(context.Context, procinfo.Executable) (string, bool) { return "1.5.14", true }
	ctx, stateDir := context.Background(), b.TempDir()
	if antigravityVersion(ctx, stateDir) != "1.5.14" {
		b.Skip("this system cannot tell a process's file")
	}
	askVersion = func(context.Context, procinfo.Executable) (string, bool) { b.Fatal("asked on a hit"); return "", false }
	for b.Loop() {
		antigravityVersion(ctx, stateDir)
	}
}
