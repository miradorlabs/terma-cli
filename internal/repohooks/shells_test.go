package repohooks

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/gitx"
)

// The scripts terma installs run under any POSIX sh — Git for Windows' bundled one
// included — so they add no dependency beyond git. CI sets TERMA_SHIM_SHELLS to dash and
// busybox; locally the test uses sh.
func TestGitHookScriptsRunUnderPOSIXShells(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake terma and displaced hooks are sh scripts")
	}
	shells := []string{"sh"}
	if s := os.Getenv("TERMA_SHIM_SHELLS"); s != "" {
		shells = strings.Split(s, ",")
	}
	for _, sh := range shells {
		for _, name := range names {
			t.Run(sh+"/"+name, func(t *testing.T) {
				for _, tc := range []struct {
					what string
					// prev is the displaced hook's body, "" for none.
					prev string
					// replaying is the sequencer file to leave in the git directory.
					replaying string
					// gitfile points the checkout's .git at the git directory, as a linked
					// worktree does, instead of git naming it in GIT_DIR.
					gitfile bool
					noTerma bool
					status  int
					want    []string
				}{
					{what: "terma alone", status: 0, want: []string{"terma hook " + name + " one two"}},
					{what: "the displaced hook first", prev: "exit 0", status: 0,
						want: []string{"prev one two", "terma hook " + name + " one two"}},
					{what: "the displaced hook's veto", prev: "exit 3", status: 3, want: []string{"prev one two"}},
					{what: "a rebase", prev: "exit 0", replaying: "rebase-merge", status: 0, want: []string{"prev one two"}},
					{what: "a cherry-pick", replaying: "CHERRY_PICK_HEAD", status: 0, want: nil},
					{what: "a cherry-picked range", replaying: "sequencer", status: 0, want: nil},
					{what: "a rebase in a linked worktree", replaying: "rebase-merge", gitfile: true, status: 0, want: nil},
					{what: "terma gone", noTerma: true, prev: "exit 0", status: 0, want: []string{"prev one two"}},
				} {
					t.Run(tc.what, func(t *testing.T) {
						dir, log := t.TempDir(), filepath.Join(t.TempDir(), "log")
						record := func(who string) string {
							return "#!/bin/sh\necho '" + who + "' \"$@\" >> '" + log + "'\n"
						}
						terma := filepath.Join(t.TempDir(), "terma")
						if !tc.noTerma {
							writeExec(t, terma, record("terma"))
						}
						writeExec(t, filepath.Join(dir, name), script(name, terma))
						if tc.prev != "" {
							writeExec(t, filepath.Join(dir, name+preTermaSuffix), record("prev")+tc.prev+"\n")
						}
						// git runs a hook from the top of the working tree, where the git
						// directory is `.git` or the file naming it.
						work, gitDir := t.TempDir(), filepath.Join(t.TempDir(), "gitdir")
						if err := os.MkdirAll(gitDir, 0o755); err != nil {
							t.Fatal(err)
						}
						if tc.replaying != "" {
							writeExec(t, filepath.Join(gitDir, tc.replaying), "")
						}
						env := append(os.Environ(), "GIT_DIR="+gitDir)
						if tc.gitfile {
							if err := os.WriteFile(filepath.Join(work, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
								t.Fatal(err)
							}
							env = append(os.Environ(), "GIT_DIR=")
						}
						args := append(strings.Fields(sh), filepath.Join(dir, name), "one", "two")
						cmd := exec.Command(args[0], args[1:]...)
						cmd.Dir, cmd.Env = work, env
						out, err := cmd.CombinedOutput()
						if got := cmd.ProcessState.ExitCode(); got != tc.status {
							t.Fatalf("%s exited %d, want %d (%v)\n%s", name, got, tc.status, err, out)
						}
						if len(out) > 0 {
							t.Errorf("%s wrote to a hook's output, which can reach the model:\n%s", name, out)
						}
						var ran []string
						if data, err := os.ReadFile(log); err == nil {
							ran = strings.Split(strings.TrimSpace(string(data)), "\n")
						}
						want := tc.want
						// A push during a stopped rebase is a real push: pre-push has no replay guard.
						if name == prePush && tc.replaying != "" {
							want = nil
							if tc.prev != "" {
								want = append(want, "prev one two")
							}
							want = append(want, "terma hook "+name+" one two")
						}
						if strings.Join(ran, "|") != strings.Join(want, "|") {
							t.Errorf("ran %q, want %q", ran, want)
						}
					})
				}
			})
		}
	}
}

// The replay guard reads the sequencer's state from the git directory GIT_DIR names, so
// it must be the linked worktree's own and not the common one the hooks live in: that is
// where git keeps rebase-merge and CHERRY_PICK_HEAD. This pins git's side of it.
func TestGitNamesALinkedWorktreesOwnGitDirToItsHooks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the probe hook is an sh script")
	}
	root, gitDir := scratch(t)
	git(t, root, "commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, root, "worktree", "add", "-q", "-b", "probe", wt)
	log := filepath.Join(t.TempDir(), "gitdir")
	// The hooks directory is the common one, which both checkouts run from.
	writeExec(t, hookPath(gitDir, "prepare-commit-msg"), "#!/bin/sh\nprintf '%s' \"$GIT_DIR\" > '"+log+"'\n")
	git(t, wt, "config", "user.email", "dev@example.com")
	git(t, wt, "config", "user.name", "Dev")
	git(t, wt, "commit", "-q", "--allow-empty", "-m", "in the worktree")

	named := readFile(t, log)
	if named == "" {
		t.Fatal("git left GIT_DIR unset for a linked worktree's hook; the guard then reads the .git file instead")
	}
	_, wtGitDir, ok := gitx.LocateFS(wt)
	if !ok {
		t.Fatal("no git directory for the worktree")
	}
	if !filepath.IsAbs(named) {
		named = filepath.Join(wt, named)
	}
	if resolve(t, named) != resolve(t, wtGitDir) {
		t.Fatalf("GIT_DIR = %q, want the worktree's own %q, where its sequencer state is", named, wtGitDir)
	}
	// And the sequencer's state is indeed private to it, not in the common directory.
	if resolve(t, wtGitDir) == resolve(t, gitDir) {
		t.Fatal("the worktree shares the common git directory, so this proves nothing")
	}
}

// git itself runs the installed hooks: the one terma displaced rewrites the message
// first, terma adds its trailer after it, a veto stops the commit, and a replayed commit
// reaches terma not at all.
func TestACommitRunsTheDisplacedHookThenTerma(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake terma and displaced hooks are sh scripts")
	}
	root, gitDir := scratch(t)
	stateDir := t.TempDir()
	git(t, root, "commit", "-q", "--allow-empty", "-m", "init") // a base to rebase onto
	// git calls terma as `terma hook prepare-commit-msg <message file> <source>`.
	terma := filepath.Join(t.TempDir(), "terma")
	writeExec(t, terma, "#!/bin/sh\n[ \"$2\" = prepare-commit-msg ] && echo 'Agent-Session-Id: s1' >> \"$3\"\nexit 0\n")
	writeExec(t, hookPath(gitDir, "prepare-commit-msg"), "#!/bin/sh\necho 'Jira-Id: ABC-1' >> \"$1\"\n")
	if _, err := Install(stateDir, terma, gitDir); err != nil {
		t.Fatal(err)
	}

	git(t, root, "commit", "-q", "--allow-empty", "-m", "first")
	msg := git(t, root, "log", "-1", "--format=%B")
	jira, session := strings.Index(msg, "Jira-Id: ABC-1"), strings.Index(msg, "Agent-Session-Id: s1")
	if jira < 0 || session < 0 || jira > session {
		t.Fatalf("both trailers must land, the displaced hook's first:\n%s", msg)
	}

	// A rebase replays commits that already carry their trailer: terma must not add a
	// second one. The displaced hook runs on each replay, as it would without terma.
	git(t, root, "commit", "-q", "--allow-empty", "-m", "second")
	base := git(t, root, "rev-parse", "HEAD~2")
	git(t, root, "rebase", "-q", "-f", "--onto", base, base, "HEAD")
	for _, rev := range []string{"HEAD", "HEAD~1"} {
		if got := strings.Count(git(t, root, "log", "-1", "--format=%B", rev), "Agent-Session-Id"); got != 1 {
			t.Fatalf("%s carries %d session trailers after a rebase, want 1:\n%s", rev, got, git(t, root, "log", "-1", "--format=%B", rev))
		}
	}

	// And a rebase in a linked worktree, where the sequencer's state is in the worktree's
	// own git directory and not the common one the hooks live in.
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, root, "worktree", "add", "-q", "-b", "side", wt)
	git(t, wt, "config", "user.email", "dev@example.com")
	git(t, wt, "config", "user.name", "Dev")
	git(t, wt, "commit", "-q", "--allow-empty", "-m", "in the worktree")
	if got := strings.Count(git(t, wt, "log", "-1", "--format=%B"), "Agent-Session-Id"); got != 1 {
		t.Fatalf("a commit in a linked worktree was stamped %d times, want 1", got)
	}
	git(t, wt, "rebase", "-q", "-f", "--onto", base, base, "HEAD")
	for _, rev := range []string{"HEAD", "HEAD~1", "HEAD~2"} {
		if got := strings.Count(git(t, wt, "log", "-1", "--format=%B", rev), "Agent-Session-Id"); got != 1 {
			t.Fatalf("%s in the worktree carries %d session trailers after a rebase, want 1:\n%s",
				rev, got, git(t, wt, "log", "-1", "--format=%B", rev))
		}
	}

	// The displaced hook keeps its veto.
	writeExec(t, hookPath(gitDir, "prepare-commit-msg"+preTermaSuffix), "#!/bin/sh\nexit 1\n")
	refused := exec.Command("git", "-C", root, "commit", "-q", "--allow-empty", "-m", "refused")
	if out, err := refused.CombinedOutput(); err == nil {
		t.Fatalf("the commit was not stopped by the displaced hook:\n%s", out)
	}

	// Once terma is gone its hooks only run the one each displaced.
	if err := os.Remove(terma); err != nil {
		t.Fatal(err)
	}
	writeExec(t, hookPath(gitDir, "prepare-commit-msg"+preTermaSuffix), "#!/bin/sh\necho 'Jira-Id: ABC-2' >> \"$1\"\n")
	git(t, root, "commit", "-q", "--allow-empty", "-m", "terma gone")
	if msg := git(t, root, "log", "-1", "--format=%B"); !strings.Contains(msg, "Jira-Id: ABC-2") || strings.Contains(msg, "Agent-Session-Id") {
		t.Fatalf("with terma gone, a commit must run the displaced hook and nothing else:\n%s", msg)
	}
}

// Stand-ins for the two hook managers' installs, as they treat a hook already there:
// pre-commit moves it to .legacy and runs it, refusing to recurse; lefthook moves it to
// .old and never runs it. Each writes the same script every time, as the real ones do.
const preCommitHook = `#!/bin/sh
# File generated by pre-commit: https://pre-commit.com
[ -n "$RUNNING_LEGACY" ] && { echo "bug: installed in migration mode" >&2; exit 1; }
h="${0%/*}"
if [ -x "$h/prepare-commit-msg.legacy" ]; then RUNNING_LEGACY=1 "$h/prepare-commit-msg.legacy" "$@" || exit $?; fi
exit 0
`

const lefthookHook = "#!/bin/sh\nexit 0\n"

func managerInstall(t *testing.T, gitDir, body, suffix string) {
	t.Helper()
	path := hookPath(gitDir, "prepare-commit-msg")
	if data, err := os.ReadFile(path); err == nil && string(data) != body {
		if err := os.Rename(path, path+suffix); err != nil {
			t.Fatal(err)
		}
	}
	writeExec(t, path, body)
}

// A hook manager installed before or after terma, and installed again: every commit
// succeeds and is stamped once while terma's hook is in the chain, no hook terma did not
// write is ever deleted, and commits still succeed once terma is gone.
func TestHookManagersInstallingOverTermas(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake terma and hook managers are sh scripts")
	}
	preCommit := func(t *testing.T, gitDir string) { managerInstall(t, gitDir, preCommitHook, ".legacy") }
	lefthook := func(t *testing.T, gitDir string) { managerInstall(t, gitDir, lefthookHook, ".old") }
	// The developer's own hook leaves its own trailer, so a commit shows whether it ran.
	const mine = "#!/bin/sh\necho 'Mine-Id: D-1' >> \"$1\"\n"
	for _, tc := range []struct {
		what string
		// steps run in order: "terma" is a claimed session's install ("terma-unchanged" one
		// that writes nothing, "terma-taken" one refused), "commit:N" a commit
		// that must succeed with N session trailers, "refused" one pre-commit refuses,
		// "mine" the developer's own hook.
		steps []string
		// kept is a file that must still hold the developer's hook at the end.
		kept string
		// mineRuns has every commit run the developer's hook: a manager that chains to
		// what it replaced keeps it in the chain.
		mineRuns bool
	}{
		{what: "pre-commit after terma", steps: []string{"terma", "pre-commit", "commit:1", "terma", "commit:1"}},
		// The reinstall chains the two scripts to each other until the next session: commits
		// fail loudly, with pre-commit's own advice, rather than drop a hook silently.
		{what: "pre-commit before terma, installed again", steps: []string{"pre-commit", "terma", "commit:1", "pre-commit", "refused", "terma", "commit:1"}},
		// pre-commit runs terma's script from .legacy, and terma's runs the developer's: a
		// working chain, which the next session leaves exactly as it is.
		{what: "pre-commit over the developer's own hook", steps: []string{"mine", "terma", "commit:1", "pre-commit", "commit:1", "terma-unchanged", "commit:1"},
			kept: "prepare-commit-msg" + preTermaSuffix, mineRuns: true},
		// lefthook moves pre-commit's hook to .old, so terma's copy at .legacy no longer runs.
		{what: "lefthook over pre-commit over terma", steps: []string{"terma", "pre-commit", "lefthook", "terma", "commit:1"}},
		{what: "lefthook after terma", steps: []string{"terma", "lefthook", "commit:0", "terma", "commit:1"}},
		{what: "lefthook over the developer's own hook", steps: []string{"mine", "terma", "lefthook", "terma-taken", "commit:0"},
			kept: "prepare-commit-msg" + preTermaSuffix},
	} {
		t.Run(tc.what, func(t *testing.T) {
			root, gitDir := scratch(t)
			stateDir := t.TempDir()
			terma := filepath.Join(t.TempDir(), "terma")
			writeExec(t, terma, "#!/bin/sh\n[ \"$2\" = prepare-commit-msg ] && echo 'Agent-Session-Id: s1' >> \"$3\"\nexit 0\n")
			for i, step := range tc.steps {
				switch {
				case step == "mine":
					writeExec(t, hookPath(gitDir, "prepare-commit-msg"), mine)
				case step == "terma":
					if _, err := Install(stateDir, terma, gitDir); err != nil {
						t.Fatalf("step %d: Install: %v", i, err)
					}
				case step == "terma-unchanged":
					if changed, err := Install(stateDir, terma, gitDir); err != nil || changed {
						t.Fatalf("step %d: Install = %v, %v; want nothing written", i, changed, err)
					}
				case step == "terma-taken":
					if _, err := Install(stateDir, terma, gitDir); !errors.Is(err, ErrTaken) {
						t.Fatalf("step %d: Install = %v, want ErrTaken", i, err)
					}
				case step == "pre-commit":
					preCommit(t, gitDir)
				case step == "lefthook":
					lefthook(t, gitDir)
				case step == "refused":
					cmd := exec.Command("git", "-C", root, "commit", "-q", "--allow-empty", "-m", step)
					if out, err := cmd.CombinedOutput(); err == nil {
						t.Fatalf("step %d: the commit was not refused:\n%s", i, out)
					}
				case strings.HasPrefix(step, "commit:"):
					cmd := exec.Command("git", "-C", root, "commit", "-q", "--allow-empty", "-m", step)
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("step %d: the commit failed: %v\n%s", i, err, out)
					}
					want, _ := strconv.Atoi(strings.TrimPrefix(step, "commit:"))
					msg := git(t, root, "log", "-1", "--format=%B")
					if got := strings.Count(msg, "Agent-Session-Id"); got != want {
						t.Fatalf("step %d: %d session trailers, want %d", i, got, want)
					}
					if tc.mineRuns && !strings.Contains(msg, "Mine-Id") {
						t.Fatalf("step %d: the developer's own hook did not run:\n%s", i, msg)
					}
				}
			}
			if tc.kept != "" && readFile(t, hookPath(gitDir, tc.kept)) != mine {
				t.Fatalf("the developer's hook is gone from %s", tc.kept)
			}
			// With terma gone, commits still succeed and run whatever chain is left.
			if err := os.Remove(terma); err != nil {
				t.Fatal(err)
			}
			git(t, root, "commit", "-q", "--allow-empty", "-m", "terma gone")
			if tc.mineRuns && !strings.Contains(git(t, root, "log", "-1", "--format=%B"), "Mine-Id") {
				t.Fatal("with terma gone, the developer's own hook stopped running")
			}
		})
	}
}
