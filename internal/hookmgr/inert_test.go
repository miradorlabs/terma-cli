package hookmgr

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Every line terma commits into a repository's hook manager must be a no-op on a
// machine without terma: exit 0, print nothing, leave the message alone. A colleague
// who never installed terma must not be able to tell the hooks are there. Each case
// runs the rendered line the way its manager runs it, with a terma-less PATH.
//
// Husky is the case that bit: it runs the hook file with `sh -e`, the file's exit
// status is its last line's, and when terma created the file the terma line is the
// only line. A guard with nothing after it made the guard's status the commit's.
func TestManagerLinesAreInertWithoutTerma(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not installed")
	}
	dir := t.TempDir()
	msg := filepath.Join(dir, "COMMIT_EDITMSG")
	const original = "feat: human work\n"

	husky := filepath.Join(dir, "prepare-commit-msg")
	if err := os.WriteFile(husky, []byte(withoutSystemDirs(huskyLine("prepare-commit-msg"))+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	lefthook := strings.NewReplacer("{1}", msg, "{2}", "message", "{3}", "").Replace(withoutSystemDirs(lefthookRun("prepare-commit-msg")))

	cases := []struct {
		name string
		args []string
	}{
		{"husky", []string{"sh", "-e", husky, msg, "message"}},
		{"lefthook", []string{"sh", "-c", lefthook}},
		{"pre-commit", []string{"sh", "-c", withoutSystemDirs(preCommitEntry("prepare-commit-msg")) + " " + msg + " message"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := os.WriteFile(msg, []byte(original), 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(c.args[0], c.args[1:]...)
			cmd.Dir = dir
			cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s line failed without terma: %v\n%s", c.name, err, out)
			}
			if len(out) != 0 {
				t.Fatalf("%s line printed without terma:\n%s", c.name, out)
			}
			if got, _ := os.ReadFile(msg); string(got) != original {
				t.Fatalf("%s line changed the message without terma:\n%s", c.name, got)
			}
		})
	}
}

// withoutSystemDirs drops PathFallback's Homebrew directories from a rendered line, so
// a test that needs a machine without terma is not fooled by the one a developer
// installed there. The home directory is the test's own.
func withoutSystemDirs(line string) string {
	return strings.ReplaceAll(line, ":/opt/homebrew/bin:/usr/local/bin", "")
}

// An app started from the Dock gets launchd's PATH, without ~/.local/bin, where
// install.sh puts terma. Every committed form — the agent hooks, the fallback shim and
// each manager's line — still finds it there, and passes the hook its arguments.
func TestEveryHookFindsHomeInstallWithGUIPath(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not installed")
	}
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "terma"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$HOME/invoked\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(home, "shim")
	if err := os.WriteFile(shim, []byte(ShimScript("prepare-commit-msg")), 0o755); err != nil {
		t.Fatal(err)
	}
	husky := filepath.Join(home, "husky")
	if err := os.WriteFile(husky, []byte(huskyLine("prepare-commit-msg")+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	lefthook := strings.NewReplacer("{1}", "MSG", "{2}", "message", "{3}", "").Replace(lefthookRun("prepare-commit-msg"))
	const hookArgs = "hook\nprepare-commit-msg\nMSG\nmessage\n"
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"agent", []string{"sh", "-c", HookCommand("session-start")}, "hook\nsession-start\n"},
		{"shim", []string{"sh", shim, "MSG", "message"}, hookArgs},
		{"husky", []string{"sh", "-e", husky, "MSG", "message"}, hookArgs},
		{"lefthook", []string{"sh", "-c", lefthook}, hookArgs},
		{"pre-commit", []string{"sh", "-c", preCommitEntry("prepare-commit-msg") + " MSG message"}, hookArgs},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_ = os.Remove(filepath.Join(home, "invoked"))
			cmd := exec.Command(c.args[0], c.args[1:]...)
			cmd.Dir = home
			cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "TERMA_CHAIN_HOOKS_DIR=" + t.TempDir()}
			if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
				t.Fatalf("%s failed: %v, output %q", c.name, err, out)
			}
			if got, err := os.ReadFile(filepath.Join(home, "invoked")); err != nil || string(got) != c.want {
				t.Fatalf("%s did not run the home install as expected: %q, %v", c.name, got, err)
			}
		})
	}
}

// The husky line's PATH change stays inside it: the user's own lines after it run
// with the PATH husky gave the file.
func TestHuskyLineLeavesPATHAlone(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not installed")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "post-commit")
	if err := os.WriteFile(file, []byte(huskyLine("post-commit")+"\nprintf '%s' \"$PATH\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-e", file)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir}
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != "/usr/bin:/bin" {
		t.Fatalf("PATH after the husky line: %q, %v", out, err)
	}
}

// Shims, pre-commit entries and husky lines an earlier terma committed, before the PATH
// fallback, are terma's own: install replaces them rather than refusing them as a
// user's hook, and is idempotent afterwards.
func TestPreFallbackFormsUpgradeInPlace(t *testing.T) {
	t.Run("shim", func(t *testing.T) {
		root := t.TempDir()
		for _, hook := range GitHooks {
			write(t, root, ShimDir+"/"+hook, shimScript(hook, true))
		}
		det := Detection{Manager: GitShim}
		plan, err := PlanInstall(root, det)
		if err != nil {
			t.Fatal(err)
		}
		if err := Apply(root, plan); err != nil {
			t.Fatal(err)
		}
		for _, hook := range GitHooks {
			if got := read(t, root, ShimDir+"/"+hook); got != ShimScript(hook) {
				t.Fatalf("%s shim not upgraded:\n%s", hook, got)
			}
		}
		if again, _ := PlanInstall(root, det); !again.Empty() {
			t.Fatalf("install should be idempotent after the upgrade: %+v", again.Changes)
		}
	})
	t.Run("husky", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, ".husky/post-commit", "#!/bin/sh\n"+legacyHuskyLine("post-commit")+"\nnpm test\n")
		det := Detection{Manager: Husky}
		plan, err := PlanInstall(root, det)
		if err != nil {
			t.Fatal(err)
		}
		if err := Apply(root, plan); err != nil {
			t.Fatal(err)
		}
		if got, want := read(t, root, ".husky/post-commit"), "#!/bin/sh\n"+huskyLine("post-commit")+"\nnpm test\n"; got != want {
			t.Fatalf("husky line not upgraded in place:\n%s", got)
		}
	})
	t.Run("lefthook", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "lefthook.yml", "post-commit:\n  commands:\n    terma:\n      run: "+strings.TrimPrefix(lefthookRun("post-commit"), PathFallback)+"\n")
		det := Detection{Manager: Lefthook, ConfigPath: "lefthook.yml"}
		plan, err := PlanInstall(root, det)
		if err != nil {
			t.Fatal(err)
		}
		if err := Apply(root, plan); err != nil {
			t.Fatal(err)
		}
		if got := read(t, root, "lefthook.yml"); !strings.Contains(got, "$HOME/.local/bin") {
			t.Fatalf("lefthook run not upgraded:\n%s", got)
		}
	})
	t.Run("pre-commit", func(t *testing.T) {
		root := t.TempDir()
		det := Detection{Manager: PreCommit}
		plan, err := PlanInstall(root, det)
		if err != nil {
			t.Fatal(err)
		}
		if err := Apply(root, plan); err != nil {
			t.Fatal(err)
		}
		current := read(t, root, ".pre-commit-config.yaml")
		write(t, root, ".pre-commit-config.yaml", strings.ReplaceAll(current, PathFallback, ""))
		plan, err = PlanInstall(root, det)
		if err != nil {
			t.Fatalf("an older terma entry was refused: %v", err)
		}
		if err := Apply(root, plan); err != nil {
			t.Fatal(err)
		}
		if got := read(t, root, ".pre-commit-config.yaml"); got != current {
			t.Fatalf("pre-commit entries not upgraded in place:\n%s", got)
		}
	})
}

// A repository that installed an older terma carries the older line. Re-running
// `terma install` rewrites it where it stands, keeping the user's own lines and
// their order, and is idempotent afterwards.
func TestHuskyUpgradesStaleLineInPlace(t *testing.T) {
	root := t.TempDir()
	stale := `command -v terma >/dev/null 2>&1 && { terma hook prepare-commit-msg "$@" || true; } # ` + Marker
	write(t, root, ".husky/prepare-commit-msg", "#!/bin/sh\n"+stale+"\nnpx commitlint --edit \"$1\"\n")
	det := Detection{Manager: Husky}
	plan, err := PlanInstall(root, det)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	want := "#!/bin/sh\n" + huskyLine("prepare-commit-msg") + "\nnpx commitlint --edit \"$1\"\n"
	if got := read(t, root, ".husky/prepare-commit-msg"); got != want {
		t.Fatalf("stale line not upgraded in place:\n%s", got)
	}
	if again, _ := PlanInstall(root, det); !again.Empty() {
		t.Fatalf("install should be idempotent after the upgrade: %+v", again.Changes)
	}
}

func TestLefthookUpgradesStaleRunInPlace(t *testing.T) {
	root := t.TempDir()
	write(t, root, "lefthook.yml", strings.Join([]string{
		"prepare-commit-msg:",
		"  commands:",
		"    terma:",
		"      run: terma hook prepare-commit-msg {1} {2} {3} || true",
		"      skip: [merge, rebase]",
		"post-commit:",
		"  commands:",
		"    lint:",
		"      run: npm run lint",
		"    terma:",
		"      run: terma hook post-commit || true",
		"      skip: [merge, rebase]",
		"",
	}, "\n"))
	det := Detection{Manager: Lefthook, ConfigPath: "lefthook.yml"}
	plan, err := PlanInstall(root, det)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	got := read(t, root, "lefthook.yml")
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatal(err)
	}
	for _, hook := range GitHooks {
		entry := doc[hook].(map[string]any)["commands"].(map[string]any)["terma"].(map[string]any)
		if run := entry["run"].(string); run != lefthookRun(hook) {
			t.Fatalf("%s run not upgraded: %q", hook, run)
		}
	}
	if !strings.Contains(got, "npm run lint") {
		t.Fatalf("user command lost:\n%s", got)
	}
	if again, _ := PlanInstall(root, det); !again.Empty() {
		t.Fatalf("install should be idempotent after the upgrade: %+v", again.Changes)
	}
}

// A shim carries its format and a digest of the rest of the file. Install upgrades a
// sealed shim of an older format, keeps one of a newer format as it is — so a colleague
// on an older terma neither refuses it nor writes it back down — and refuses one whose
// digest no longer matches, since that is a developer's edit.
func TestSealedShims(t *testing.T) {
	det := Detection{Manager: GitShim}
	install := func(t *testing.T, content func(hook string) string) (string, error) {
		t.Helper()
		root := t.TempDir()
		for _, hook := range GitHooks {
			write(t, root, ShimDir+"/"+hook, content(hook))
		}
		plan, err := PlanInstall(root, det)
		if err != nil {
			return "", err
		}
		if err := Apply(root, plan); err != nil {
			t.Fatal(err)
		}
		if again, _ := PlanInstall(root, det); !again.Empty() {
			t.Fatalf("install should be idempotent: %+v", again.Changes)
		}
		return read(t, root, ShimDir+"/post-commit"), nil
	}

	if format, ok := sealedFormat([]byte(ShimScript("post-commit"))); !ok || format != shimFormat {
		t.Fatalf("this build's shim is not sealed with its own format: %d, %v", format, ok)
	}

	older := func(hook string) string { return seal("#!/bin/sh\nterma hook "+hook+" \"$@\" || true\n", shimFormat-1) }
	if got, err := install(t, older); err != nil || got != ShimScript("post-commit") {
		t.Fatalf("an older sealed shim was not upgraded: %v\n%s", err, got)
	}

	newer := func(hook string) string {
		return seal("#!/bin/sh\n# from a later terma\nterma hook "+hook+" \"$@\" || true\n", shimFormat+1)
	}
	if got, err := install(t, newer); err != nil || got != newer("post-commit") {
		t.Fatalf("a newer sealed shim was not kept as it is: %v\n%s", err, got)
	}

	edited := func(hook string) string {
		return strings.Replace(ShimScript(hook), "exit 0\n", "npm test\nexit 0\n", 1)
	}
	if _, err := install(t, edited); err == nil || !strings.Contains(err.Error(), "unrecognized or modified") {
		t.Fatalf("an edited sealed shim was not refused: %v", err)
	}
}
