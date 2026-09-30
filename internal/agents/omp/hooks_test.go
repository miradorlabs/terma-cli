package omp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hookrun/hookruntest"
)

func TestOmpHooksInstallUninstallRoundTrip(t *testing.T) {
	root := t.TempDir()

	plan, err := planHooks(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Empty() {
		t.Fatal("install on an empty repository must plan the hook file")
	}
	if len(plan.Changes) != 1 || plan.Changes[0].Path != hooksPath || plan.Changes[0].Before != nil {
		t.Fatalf("plan = %+v", plan.Changes)
	}
	if err := hookmgr.Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	got := hookruntest.ReadFile(t, root, hooksPath)
	if got != ompHooksSource {
		t.Fatal("installed file differs from the embedded source")
	}

	// Install is idempotent: the file already matches the render.
	again, err := planHooks(root, true)
	if err != nil || !again.Empty() {
		t.Fatalf("reinstall plan = %+v, %v", again, err)
	}

	// Uninstall removes exactly that file.
	un, err := planHooks(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(un.Changes) != 1 || un.Changes[0].After != nil {
		t.Fatalf("uninstall plan = %+v", un.Changes)
	}
	if err := hookmgr.Apply(root, un); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(hooksPath))); err == nil {
		t.Fatal("hook file still present after uninstall")
	}
}

// A hook file somebody edited is theirs: uninstall leaves it alone and says why.
func TestOmpHooksUninstallKeepsAnEditedFile(t *testing.T) {
	root := t.TempDir()
	plan, _ := planHooks(root, true)
	if err := hookmgr.Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	edited := ompHooksSource + "\n// local tweak\n"
	hookruntest.WriteFile(t, root, hooksPath, edited)

	un, err := planHooks(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if !un.Empty() {
		t.Fatalf("an edited file must not be touched: %+v", un.Changes)
	}
	if len(un.Notes) != 1 || !strings.Contains(un.Notes[0], "local edits") {
		t.Fatalf("no note explaining the kept file: %v", un.Notes)
	}
	if got := hookruntest.ReadFile(t, root, hooksPath); got != edited {
		t.Fatal("edited hook file was modified")
	}

	// Reinstalling rewrites terma's file in place, edits and all.
	re, err := planHooks(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(re.Changes) != 1 {
		t.Fatalf("reinstall plan = %+v", re.Changes)
	}
	if err := hookmgr.Apply(root, re); err != nil {
		t.Fatal(err)
	}
	if got := hookruntest.ReadFile(t, root, hooksPath); got != ompHooksSource {
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
	plan, err := planHooks(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Empty() {
		t.Fatalf("plan = %+v", plan.Changes)
	}
}
