// Package repohooks installs terma's git hooks into a repository's own
// .git/hooks, on demand. Nothing in terma takes them out: once terma is gone they only run
// the hook each one displaced. git runs exactly one file per hook
// name, so a hook already there is renamed aside and run first, unchanged — the chain
// pattern the pre-commit framework established — and terma recognizes its own script by
// a marker, so a reinstall never chains a script to itself.
package repohooks

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/gitx"
)

// names are the hooks terma installs, the only ones with anything to do: one stamps the
// message, one records the commit, and one records what a push sends.
var names = []string{"prepare-commit-msg", "post-commit", "pre-push"}

// stdinHooks are the hooks git hands input on stdin, which both the displaced hook and terma
// must read.
var stdinHooks = map[string]bool{"pre-push": true}

// preTermaSuffix names the hook terma displaced, which its script runs first.
const preTermaSuffix = ".pre-terma"

// hooksDir is git's own name for a repository's hooks directory.
const hooksDir = "hooks"

// templateVersion marks the script terma writes. Bumping it rewrites every installed
// copy at the next session, which is how a template fix reaches them.
const templateVersion = 1

// marker starts the script's second line and is how terma knows its own file.
const marker = "# terma-hook v"

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// replayState are the files git leaves in the git directory while it replays commits —
// a rebase, a cherry-pick, or either stopped at a conflict. GIT_REFLOG_ACTION is not in a
// hook's environment, so this is what tells a replay from a commit someone is making.
var replayState = []string{"CHERRY_PICK_HEAD", "rebase-merge", "rebase-apply", "sequencer"}

// script is the POSIX sh terma writes for one hook name. It never lets a missing or
// failing terma block git; the displaced hook runs first and keeps its veto, and git's own
// arguments pass through unchanged. It forks nothing: a replayed commit must cost git no
// more than a rebase without terma, so the hook has to decide without starting a process.
// $0 is the hook's path, which is how the displaced hook is found beside it, and git names
// the git directory in GIT_DIR wherever it is not the checkout's own `.git`. A hook git
// feeds on stdin reads it once into a variable with the shell's own read, and hands each
// reader a here-document of it, so neither forks a process to share it.
func script(hook, terma string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#!/bin/sh\n%s%d: chains to %s%s. Written by terma.\n",
		marker, templateVersion, hook, preTermaSuffix)
	b.WriteString("d=\"${0%/*}\"\n[ \"$d\" = \"$0\" ] && d=.\n")
	fmt.Fprintf(&b, "prev=\"$d/%s%s\"\n", hook, preTermaSuffix)
	run := ""
	if stdinHooks[hook] {
		b.WriteString("in=\nwhile IFS= read -r l || [ -n \"$l\" ]; do in=\"${in:+$in\n}$l\"; done\n")
		b.WriteString("feed() {\n  if [ -n \"$in\" ]; then \"$@\" <<EOF\n$in\nEOF\n  else \"$@\" </dev/null; fi\n}\n")
		run = "feed "
	}
	fmt.Fprintf(&b, "if [ -x \"$prev\" ]; then %s\"$prev\" \"$@\" || exit $?; fi\n", run)
	b.WriteString("g=\"${GIT_DIR:-.git}\"\n")
	b.WriteString("if [ ! -d \"$g\" ]; then IFS= read -r line < \"$g\" 2>/dev/null && g=\"${line#gitdir: }\"; fi\n")
	// Never stamp or record a commit git is replaying.
	fmt.Fprintf(&b, "for s in %s; do\n  [ -e \"$g/$s\" ] && exit 0\ndone\n", strings.Join(replayState, " "))
	fmt.Fprintf(&b, "[ -x %[1]s ] && %[3]s%[1]s hook %[2]s \"$@\" || true\nexit 0\n", shellQuote(terma), hook, run)
	return b.String()
}

// maxOurScript bounds how far terma reads a hook to tell whether it wrote it. Its own
// script is a few hundred bytes, and a hook can be a compiled binary that prepare-commit-msg
// cannot afford to read inside its budget.
const maxOurScript = 4 << 10

// isOurs reports whether data is a script terma wrote, by the marker on its second line.
// Anything over the bound cannot be, however it begins, so nothing large is ever rewritten.
func isOurs(data []byte) bool {
	if len(data) > maxOurScript {
		return false
	}
	lines := strings.SplitN(string(data), "\n", 3)
	return len(lines) > 1 && strings.HasPrefix(lines[1], marker)
}

// found reads the hook at path far enough to judge it; present is false when there is none.
func found(path string) (data []byte, present bool, err error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()
	data, err = io.ReadAll(io.LimitReader(f, maxOurScript+1))
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// hooksDirFor is the repository's own hooks directory with the git directory holding it:
// the common one, so one install serves every linked worktree.
func hooksDirFor(gitDir string) (dir, common string, err error) {
	common, err = filepath.Abs(gitx.CommonDirFS(gitDir))
	if err != nil {
		return "", "", err
	}
	// One repository must be recorded under one path however a session reached it: a linked
	// worktree's gitfile can name the common directory through a symlink the cwd did not.
	if resolved, err := filepath.EvalSymlinks(common); err == nil {
		common = resolved
	}
	return filepath.Join(common, hooksDir), common, nil
}

// ErrTaken is Install's answer when another tool's hook has replaced terma's and the hook
// terma set aside before is still there: terma deletes no hook it did not write, so it
// leaves the repository alone.
var ErrTaken = errors.New("another tool's hook has replaced terma's, and the hook terma set aside is still there")

// Install writes terma's commit hooks into the repository's own hooks directory; hooks
// already there and current are left as they are. Per name: nothing there gets terma's
// script; terma's own is rewritten only when it has changed; anyone else's is renamed to
// <name>.pre-terma, and any copy of terma's script that tool moved aside goes. A
// <name>.pre-terma already there is replaced only by an identical copy, a hook manager
// reinstalling itself; anything else is ErrTaken, and nothing is written. stateDir holds
// the lock that serializes installs.
func Install(stateDir, terma, gitDir string) (changed bool, err error) {
	if current(gitDir, terma) {
		return false, nil
	}
	// Two sessions starting at once must not both rename the hook already there: the second
	// rename would put terma's script over the one the first set aside.
	err = flock.Locked(lockPath(stateDir), lockWait, func() error {
		changed, err = install(terma, gitDir)
		return err
	})
	return changed, err
}

// current reports whether all of terma's hooks are already its script at this version,
// at the name or where a tool that chains to it runs it from: the common case, which needs
// no lock.
func current(gitDir, terma string) bool {
	dir, hooks, ok := states(gitDir)
	if !ok {
		return false
	}
	for _, name := range names {
		path := filepath.Join(dir, name)
		switch h := hooks[name]; h.state {
		case hookOurs:
		case hookViaTool:
			path = h.copy
		default:
			return false
		}
		there, present, err := found(path)
		if err != nil || !present || !bytes.Equal(there, []byte(script(name, terma))) {
			return false
		}
	}
	return true
}

// lockPath serializes every change terma makes to repositories' hooks.
func lockPath(stateDir string) string { return filepath.Join(stateDir, "repo-hooks-install") }

// lockWait bounds the wait for the lock: installs finish in milliseconds, and a hook must
// not keep its agent waiting.
const lockWait = 2 * time.Second

func install(terma, gitDir string) (changed bool, err error) {
	dir, _, err := hooksDirFor(gitDir)
	if err != nil {
		return false, err
	}
	if Taken(gitDir) {
		return false, ErrTaken
	}
	// git creates hooks/ at init, but a repository can be without it.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	for _, name := range names {
		wrote, err := installOne(dir, name, terma)
		changed = changed || wrote
		if err != nil {
			return changed, err
		}
	}
	return changed, nil
}

func installOne(dir, name, terma string) (bool, error) {
	path := filepath.Join(dir, name)
	want := []byte(script(name, terma))
	h, err := stateOf(dir, name)
	if err != nil {
		return false, err
	}
	switch h.state {
	case hookViaTool:
		// pre-commit runs terma's script from .legacy: only a stale copy is rewritten, there.
		if there, _, err := found(h.copy); err == nil && bytes.Equal(there, want) {
			return false, nil
		}
		return true, config.WriteFileAtomic(h.copy, want, 0o755)
	case hookOurs:
		if there, _, err := found(path); err == nil && bytes.Equal(there, want) {
			return false, nil
		}
	case hookTaken:
		return false, ErrTaken
	case hookOther, hookLooped:
		// A tool that installed over terma's script moved it aside to chain to it; terma's
		// script at the name runs that tool's now, so the copy would only run terma twice.
		if err := removeOurCopies(dir, name); err != nil {
			return false, err
		}
		// Taken has made sure any hook already at .pre-terma is a copy of this one.
		if err := config.Rename(path, path+preTermaSuffix); err != nil {
			return false, err
		}
	}
	return true, config.WriteFileAtomic(path, want, 0o755)
}

// Taken reports whether, in the repository at gitDir, another tool's hook has replaced
// terma's without running it, and the hook terma set aside before is still there and
// differs from it.
func Taken(gitDir string) bool {
	_, hooks, ok := states(gitDir)
	if !ok {
		return false
	}
	for _, h := range hooks {
		if h.state == hookTaken {
			return true
		}
	}
	return false
}

// sameBytes reports whether the files at a and b hold the same bytes.
func sameBytes(a, b string) bool {
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	if errA != nil || errB != nil || ia.Size() != ib.Size() {
		return false
	}
	da, errA := os.ReadFile(a)
	db, errB := os.ReadFile(b)
	return errA == nil && errB == nil && bytes.Equal(da, db)
}

// removeOurCopies deletes the copies of terma's script that another tool moved aside
// under its own suffix (pre-commit's .legacy, lefthook's .old).
func removeOurCopies(dir, name string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		n := e.Name()
		if !strings.HasPrefix(n, name+".") || n == name+preTermaSuffix || e.IsDir() {
			continue
		}
		if data, present, err := found(filepath.Join(dir, n)); err == nil && present && isOurs(data) {
			if err := config.Remove(filepath.Join(dir, n)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Installed reports whether all of terma's hooks are in place in the repository at
// gitDir, and whether a hook runs before them: one that was there first, or a tool that
// installed over terma's and runs its script (pre-commit's migration mode).
func Installed(gitDir string) (installed, chained bool) {
	dir, hooks, ok := states(gitDir)
	if !ok {
		return false, false
	}
	for _, name := range names {
		switch hooks[name].state {
		case hookViaTool:
			chained = true
		case hookOurs:
			if _, err := os.Lstat(filepath.Join(dir, name+preTermaSuffix)); err == nil {
				chained = true
			}
		default:
			return false, false
		}
	}
	return true, chained
}
