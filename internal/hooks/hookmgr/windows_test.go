//go:build windows

package hookmgr

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain doubles as a stub terma: with TERMA_STUB_OUT set, it records its arguments and
// stdin there and exits 2, the status with which Codex blocks, as a crashed Go binary does.
func TestMain(m *testing.M) {
	if out := os.Getenv("TERMA_STUB_OUT"); out != "" {
		in, _ := io.ReadAll(os.Stdin)
		_ = os.WriteFile(out, []byte(strings.Join(os.Args[1:], " ")+"\n"+string(in)), 0o600)
		os.Exit(2)
	}
	os.Exit(m.Run())
}

// The shells Codex runs a Windows hook line with (codex-rs hooks/src/engine/command_runner.rs
// build_command, core/src/shell.rs derive_exec_args): the session's PowerShell with
// -NoProfile -Command, or cmd /C with the line wrapped in quotes as a raw argument.
func codexShells(t *testing.T) map[string]func(line string) *exec.Cmd {
	t.Helper()
	shells := map[string]func(string) *exec.Cmd{
		"cmd": func(line string) *exec.Cmd {
			comspec := os.Getenv("COMSPEC")
			c := exec.Command(comspec)
			c.SysProcAttr = &syscall.SysProcAttr{CmdLine: `"` + comspec + `" /C "` + line + `"`}
			return c
		},
	}
	for _, ps := range []string{"powershell", "pwsh"} {
		path, err := exec.LookPath(ps)
		if err != nil {
			// Windows PowerShell ships with Windows; pwsh is optional.
			if ps == "powershell" {
				t.Fatal(err)
			}
			t.Logf("%s: not installed", ps)
			continue
		}
		shells[ps] = func(line string) *exec.Cmd { return exec.Command(path, "-NoProfile", "-Command", line) }
	}
	return shells
}

// The line setup writes as commandWindows runs terma with the hook's stdin under every shell
// Codex may use and exits 0 whatever terma exits with, and does nothing, successfully and
// silently, once terma is gone.
func TestWindowsHookCommandRunsUnderCodexShells(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "Jo O'Neil (dev) é")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	terma := filepath.Join(dir, "terma.exe")
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(terma, data, 0o755); err != nil {
		t.Fatal(err)
	}
	const payload = `{"session_id":"s-1","hook_event_name":"Stop"}`

	for name, shell := range codexShells(t) {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "stub.out")
			t.Setenv("TERMA_STUB_OUT", out)

			line, ok := WindowsHookCommand(SystemCmd(), UserHookCommand(terma)("codex-stop"))
			if !ok {
				t.Fatal("no Windows command")
			}
			c := shell(line)
			c.Stdin = strings.NewReader(payload)
			var output bytes.Buffer
			c.Stdout, c.Stderr = &output, &output
			start := time.Now()
			if err := c.Run(); err != nil {
				t.Fatalf("%v: %s", err, output.String())
			}
			t.Logf("ran terma in %v", time.Since(start))
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("terma did not run: %v; output %q", err, output.String())
			}
			// cmd /C hands terma the backtick PowerShell would have consumed, which it ignores.
			args, stdin, _ := strings.Cut(string(got), "\n")
			if strings.TrimSuffix(args, " `") != "hook --user codex-stop" || stdin != payload {
				t.Fatalf("terma got %q", got)
			}

			gone, _ := WindowsHookCommand(SystemCmd(), UserHookCommand(filepath.Join(dir, "gone.exe"))("codex-stop"))
			c = shell(gone)
			c.Stdin = strings.NewReader(payload)
			output.Reset()
			c.Stdout, c.Stderr = &output, &output
			if err := c.Run(); err != nil || output.Len() > 0 {
				t.Fatalf("with terma gone: %v, output %q", err, output.String())
			}
		})
	}
}
