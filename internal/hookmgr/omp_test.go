package hookmgr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOmpHooksInstallUninstallRoundTrip(t *testing.T) {
	root := t.TempDir()

	plan, err := PlanOmpHooks(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Empty() {
		t.Fatal("install on an empty repository must plan the hook file")
	}
	if len(plan.Changes) != 1 || plan.Changes[0].Path != OmpHooksPath || plan.Changes[0].Before != nil {
		t.Fatalf("plan = %+v", plan.Changes)
	}
	if err := Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	got := read(t, root, OmpHooksPath)
	if got != ompHooksSource {
		t.Fatal("installed file differs from the embedded source")
	}

	// Install is idempotent: the file already matches the render.
	again, err := PlanOmpHooks(root, true)
	if err != nil || !again.Empty() {
		t.Fatalf("reinstall plan = %+v, %v", again, err)
	}

	// Uninstall removes exactly that file.
	un, err := PlanOmpHooks(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(un.Changes) != 1 || un.Changes[0].After != nil {
		t.Fatalf("uninstall plan = %+v", un.Changes)
	}
	if err := Apply(root, un); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(OmpHooksPath))); err == nil {
		t.Fatal("hook file still present after uninstall")
	}
}

// A hook file somebody edited is theirs: uninstall leaves it alone and says why.
func TestOmpHooksUninstallKeepsAnEditedFile(t *testing.T) {
	root := t.TempDir()
	plan, _ := PlanOmpHooks(root, true)
	if err := Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	edited := ompHooksSource + "\n// local tweak\n"
	write(t, root, OmpHooksPath, edited)

	un, err := PlanOmpHooks(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if !un.Empty() {
		t.Fatalf("an edited file must not be touched: %+v", un.Changes)
	}
	if len(un.Notes) != 1 || !strings.Contains(un.Notes[0], "local edits") {
		t.Fatalf("no note explaining the kept file: %v", un.Notes)
	}
	if got := read(t, root, OmpHooksPath); got != edited {
		t.Fatal("edited hook file was modified")
	}

	// Reinstalling rewrites terma's file in place, edits and all.
	re, err := PlanOmpHooks(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(re.Changes) != 1 {
		t.Fatalf("reinstall plan = %+v", re.Changes)
	}
	if err := Apply(root, re); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, OmpHooksPath); got != ompHooksSource {
		t.Fatal("reinstall did not restore the render")
	}
}

// The committed hook names its events in a comment, which is also what the
// adapter/contract test greps for: the two must stay in step with the handlers.
func TestOmpHooksSourceNamesItsEvents(t *testing.T) {
	for _, event := range []string{"omp-session-start", "omp-session-end", "omp-file-edit"} {
		if !strings.Contains(ompHooksSource, "terma hook "+event) {
			t.Errorf("hook source does not name %s", event)
		}
	}
	// omp's hook loader treats every export as a hook factory; a stray helper export
	// would make the hook fail to load.
	exports := 0
	for line := range strings.SplitSeq(ompHooksSource, "\n") {
		if strings.HasPrefix(line, "export ") {
			exports++
		}
	}
	if exports != 1 {
		t.Fatalf("hook source has %d exports, want exactly 1", exports)
	}
}

func TestOmpHooksUninstallFromNothingIsANoop(t *testing.T) {
	plan, err := PlanOmpHooks(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Empty() {
		t.Fatalf("plan = %+v", plan.Changes)
	}
}
