//go:build unix

package shim

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A PTY is necessary here: Process.Signal(SIGINT) alone does not exercise job
// control, the terminal foreground process group, or the shell's return to a prompt.
func TestLauncherInteractiveTerminal(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("python3 is required for terminal integration tests")
	}
	driver, err := filepath.Abs("testdata/launcher_pty.py")
	if err != nil {
		t.Fatal(err)
	}
	for _, shell := range []string{"/bin/sh", "/bin/bash", "/bin/zsh", "/bin/dash"} {
		if _, err := os.Stat(shell); err != nil {
			continue
		}
		for _, phase := range []string{"preparation", "agent"} {
			for _, activation := range []string{"path", "function"} {
				t.Run(filepath.Base(shell)+"/"+phase+"/"+activation, func(t *testing.T) {
					prep := "printf 'terma-args-v1:0\\n' > \"$4/count\"\n"
					if phase == "preparation" {
						prep = "echo ready > \"$MARKER\"\nexec /bin/sleep 60\n"
					}
					_, realDir, launcher := launcherFixture(t, prep)
					if activation == "function" {
						setup := filepath.Join(realDir, "setup.sh")
						if err := os.WriteFile(setup, []byte(WrapperSnippet([]string{AgentClaude})), 0600); err != nil {
							t.Fatal(err)
						}
						t.Setenv("TERMA_PTY_SETUP", setup)
						launcher = AgentClaude
					} else {
						t.Setenv("TERMA_PTY_SETUP", "")
					}
					marker := filepath.Join(realDir, "ready")
					t.Setenv("MARKER", marker)
					temporary := t.TempDir()
					t.Setenv("TMPDIR", temporary)
					script := `#!/bin/sh
[ -t 0 ] && [ -t 1 ] && [ -t 2 ] || exit 91
echo started > "$MARKER.agent"
trap 'exit 130' INT
trap 'printf "RESIZED:"; /bin/stty size' WINCH
printf 'AGENT_READY\n'
IFS= read -r reply
printf 'READ:%s\n' "$reply"
while :; do IFS= read -r reply; done
`
					if err := os.WriteFile(filepath.Join(realDir, AgentClaude), []byte(script), 0755); err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, python, driver, shell, launcher, phase, marker, temporary)
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("interactive terminal: %v\n%s", err, out)
					}
				})
			}
		}
	}
}
