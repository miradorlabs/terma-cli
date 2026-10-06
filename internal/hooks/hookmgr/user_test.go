package hookmgr

import (
	"testing"
)

func TestUserHookCommandShape(t *testing.T) {
	t.Parallel()
	cmd := UserHookCommand("/Users/dev/.local/bin/terma")("session-start")
	if cmd != `[ -x '/Users/dev/.local/bin/terma' ] && '/Users/dev/.local/bin/terma' hook --user session-start || true` {
		t.Fatalf("command = %s", cmd)
	}
	if !ownedHookCommand(cmd) || !ownedHookCommand(UserHookCommand("/a/it's/terma")("codex-stop")) {
		t.Fatal("a machine-wide entry is not recognized as terma's")
	}
	if ownedHookCommand(`[ -x '/bin/x' ] && '/bin/x' hook --user a; rm -rf / || true`) {
		t.Fatal("a command that only resembles terma's was taken for one")
	}
}

// The managed files an organization deploys hold every one of terma's hooks, each
// calling terma through $HOME, and parse as the agents read them.
func TestManagedConfig(t *testing.T) {
	t.Parallel()
	cmd := ManagedHookCommand("$HOME/.local/bin/terma")
	if got := cmd("codex-stop"); got != `[ -x "$HOME/.local/bin/terma" ] && "$HOME/.local/bin/terma" hook --user codex-stop || true` || !ownedHookCommand(got) {
		t.Fatalf("managed command %q", got)
	}
}

func TestWindowsHookCommandShape(t *testing.T) {
	t.Parallel()
	const cmd = `C:\Windows\System32\cmd.exe`
	terma := `C:\Users\Jo O'Neil\terma (dev)\terma.exe`
	if _, ok := WindowsHookCommand(cmd, UserHookCommand(`C:\Program Files (x86)\Zoë's terma\terma.exe`)("codex-stop")); !ok {
		t.Fatal("refused a plain path")
	}
	got, ok := WindowsHookCommand(cmd, UserHookCommand(terma)("codex-stop"))
	if want := cmd + ` /d /c if exist "` + terma + `" "` + terma + `" hook --user codex-stop ` + "`& exit 0"; !ok || got != want {
		t.Fatalf("got %q, %v; want %q", got, ok, want)
	}
	for _, other := range []string{
		ManagedHookCommand("$HOME/.local/bin/terma")("codex-stop"),
		`[ -x '/bin/a' ] && '/bin/b' hook --user codex-stop || true`,
		`[ -x '/bin/a' ] && '/bin/a' hook --user codex-stop; rm -rf / || true`,
		UserHookCommand(`C:\Users\100%\terma.exe`)("codex-stop"),
		UserHookCommand(`C:\Users\$mith\terma.exe`)("codex-stop"),
		UserHookCommand(`C:\Users\!NAME!\terma.exe`)("codex-stop"),
		UserHookCommand("C:\\Users\\a`b\\terma.exe")("codex-stop"),
		UserHookCommand(`C:\a&b\terma.exe`)("codex-stop"),
		UserHookCommand(`C:\a (1)&b\terma.exe`)("codex-stop"),
		UserHookCommand(`C:\x(1)\terma.exe`)("codex-stop"),
		UserHookCommand(`C:\Users\Jo “Dev”\terma.exe`)("codex-stop"),
	} {
		if _, ok := WindowsHookCommand(cmd, other); ok {
			t.Fatalf("a Windows command made from %q", other)
		}
	}
	// Only an unquoted absolute interpreter leads the line.
	for _, bad := range []string{`C:\Program Files\cmd.exe`, `C:\W%x%\cmd.exe`, `C:\Jo's\cmd.exe`, `C:\{w}\cmd.exe`, `cmd.exe`, `\\srv\share\cmd.exe`} {
		if _, ok := WindowsHookCommand(bad, UserHookCommand(terma)("codex-stop")); ok {
			t.Fatalf("a Windows command led by %q", bad)
		}
	}
}

func TestSystemCmd(t *testing.T) {
	t.Setenv("SystemRoot", `D:\Win`)
	if got := SystemCmd(); got != `D:\Win\System32\cmd.exe` {
		t.Fatalf("SystemCmd() = %q", got)
	}
	t.Setenv("SystemRoot", "")
	if got := SystemCmd(); got != `C:\Windows\System32\cmd.exe` {
		t.Fatalf("SystemCmd() without SystemRoot = %q", got)
	}
}
