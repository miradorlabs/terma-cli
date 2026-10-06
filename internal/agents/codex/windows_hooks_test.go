package codex

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// TestMain starts every test off Windows, whatever the OS: the package's fixtures name
// POSIX paths no Windows command can be made from, and the tests below opt in.
func TestMain(m *testing.M) {
	onWindows = false
	os.Exit(m.Run())
}

// runningOnWindows makes this package write and hash entries as it does on Windows, or not.
func runningOnWindows(t *testing.T, on bool) {
	t.Helper()
	was := onWindows
	onWindows = on
	t.Cleanup(func() { onWindows = was })
}

// handlers are the handlers in the hooks file setup writes with command.
func handlers(t *testing.T, command func(string) string) []map[string]any {
	t.Helper()
	_, hooksFile := userHookedWith(t, command)
	var doc struct {
		Hooks map[string][]struct {
			Hooks []map[string]any `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(read(t, hooksFile)), &doc); err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, groups := range doc.Hooks {
		for _, g := range groups {
			out = append(out, g.Hooks...)
		}
	}
	return out
}

// On Windows every entry also carries the cmd line Codex runs there; elsewhere the entries
// are byte for byte what they were, so approvals Codex already recorded still hold.
func TestUserHooksCarryCommandWindowsOnlyOnWindows(t *testing.T) {
	terma := `C:\Users\Jo O'Neil\AppData\Local\terma\terma.exe`
	command := hookmgr.UserHookCommand(terma)

	runningOnWindows(t, false)
	for _, h := range handlers(t, command) {
		if _, ok := h["commandWindows"]; ok {
			t.Fatalf("commandWindows written off Windows: %v", h)
		}
	}

	runningOnWindows(t, true)
	got := handlers(t, command)
	if len(got) != len(codexHooks) {
		t.Fatalf("%d handlers, want %d", len(got), len(codexHooks))
	}
	for _, h := range got {
		posix := h["command"].(string)
		event := posix[strings.LastIndex(posix, "--user ")+len("--user ") : len(posix)-len(" || true")]
		want := hookmgr.SystemCmd() + ` /d /c if exist "` + terma + `" "` + terma + `" hook --user ` + event + " `& exit 0"
		if h["commandWindows"] != want || posix != command(event) {
			t.Fatalf("handler %v\nwant commandWindows %s", h, want)
		}
	}
}

// Codex hashes the command it runs: on Windows commandWindows stands in for command and is
// not hashed itself; elsewhere it is ignored.
func TestCodexEntryHashUsesTheCommandCodexRuns(t *testing.T) {
	entry := Entry{Event: "Stop"}
	hash := func(raw string) string {
		t.Helper()
		h, err := codexEntryHash(entry, nil, json.RawMessage(raw))
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	both := `{"type":"command","command":"posix","commandWindows":"windows","timeout":3}`
	runningOnWindows(t, false)
	if hash(both) != hash(`{"type":"command","command":"posix","timeout":3}`) {
		t.Fatal("commandWindows changed the hash off Windows")
	}
	runningOnWindows(t, true)
	if hash(both) != hash(`{"type":"command","command":"windows","timeout":3}`) {
		t.Fatal("on Windows the hash is not that of commandWindows run as the command")
	}
}

// Setup's approvals cover the Windows entries, so doctor reads them as trusted.
func TestWindowsEntriesAreApprovedAndTrusted(t *testing.T) {
	runningOnWindows(t, true)
	command := hookmgr.UserHookCommand(`C:\Program Files\terma\terma.exe`)
	configDir := t.TempDir()
	_, hooksFile := userHookedWith(t, command)
	done, err := Agent{ConfigDir: configDir}.SyncHookTrust(hooksFile, command)
	if err != nil || done.Approved != len(codexHooks) {
		t.Fatalf("approved %+v, %v", done, err)
	}
	if present, trusted, err := (Agent{ConfigDir: configDir}).UserHooksTrusted(); err != nil || !present || !trusted {
		t.Fatalf("present %v trusted %v, %v", present, trusted, err)
	}
}

// Setup refuses a terma path with no Windows form rather than write entries Codex cannot
// run, while teardown still removes what an earlier setup wrote.
func TestWindowsEntriesNeedAWindowsForm(t *testing.T) {
	runningOnWindows(t, true)
	unsafe := hookmgr.UserHookCommand(`C:\Users\$mith\terma.exe`)
	codexHome := t.TempDir()
	if _, err := planUserHooks(codexHome, unsafe, true); err == nil {
		t.Fatal("planned an entry with no commandWindows")
	}
	runningOnWindows(t, false)
	plan, err := planUserHooks(codexHome, unsafe, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := hookmgr.Apply(codexHome, plan); err != nil {
		t.Fatal(err)
	}
	runningOnWindows(t, true)
	plan, err = planUserHooks(codexHome, unsafe, false)
	if err != nil || len(plan.Changes) != 1 || plan.Changes[0].After != nil {
		t.Fatalf("teardown plan %+v, %v; want the hooks file removed", plan, err)
	}
}
