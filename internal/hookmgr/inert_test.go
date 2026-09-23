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
	if err := os.WriteFile(husky, []byte(huskyLine("prepare-commit-msg")+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	lefthook := strings.NewReplacer("{1}", msg, "{2}", "message", "{3}", "").Replace(lefthookRun("prepare-commit-msg"))

	cases := []struct {
		name string
		args []string
	}{
		{"husky", []string{"sh", "-e", husky, msg, "message"}},
		{"lefthook", []string{"sh", "-c", lefthook}},
		{"pre-commit", []string{"sh", "-c", preCommitEntry("prepare-commit-msg") + " " + msg + " message"}},
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
