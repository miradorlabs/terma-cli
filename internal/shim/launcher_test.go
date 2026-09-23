//go:build unix

package shim

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func launcherFixture(t *testing.T, preparation string) (string, string, string) {
	t.Helper()
	sandbox(t)
	bin, err := InstallShims([]string{AgentClaude, AgentCodex})
	if err != nil {
		t.Fatal(err)
	}
	realDir := t.TempDir()
	for _, name := range []string{AgentClaude, AgentCodex} {
		if err := os.WriteFile(filepath.Join(realDir, name), []byte("#!/bin/sh\nprintf '%s\\000' \"$@\"\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if preparation != "" {
		if err := os.WriteFile(filepath.Join(realDir, "terma"), []byte("#!/bin/sh\n"+preparation), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+realDir)
	t.Setenv("TERMA_DISABLE", "")
	return bin, realDir, filepath.Join(bin, AgentClaude)
}

func runLauncher(t *testing.T, path string, args []string) ([]string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		t.Fatal("launcher hung")
	}
	var got []string
	if len(out) > 0 {
		got = strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	}
	return got, stderr.String(), err
}

func TestLauncherPreparationFailuresPassThrough(t *testing.T) {
	for name, prep := range map[string]string{
		"missing":          "",
		"crash":            "kill -KILL $$\n",
		"hang":             "exec /bin/sleep 60\n",
		"old-version":      "exit 2\n",
		"truncated":        "printf terma-args-v1:2 > \"$4/count\"\n",
		"missing-argument": "printf 'terma-args-v1:1\\n' > \"$4/count\"\n",
		"bad-version":      "printf 'terma-args-v2:0\\n' > \"$4/count\"\n",
		"invalid-count":    "printf 'terma-args-v1:08\\n' > \"$4/count\"\n",
		"empty":            "exit 0\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, launcher := launcherFixture(t, prep)
			args := []string{"-p", "spaces ' \" $(touch NEVER) `false`", "", "line\n\n", "--", "*"}
			start := time.Now()
			got, _, err := runLauncher(t, launcher, args)
			if err != nil || !reflect.DeepEqual(got, args) {
				t.Fatalf("args=%q err=%v", got, err)
			}
			if time.Since(start) > 4*time.Second {
				t.Fatal("preparation deadline not enforced")
			}
		})
	}
}

func TestLauncherReadsArgumentsAsData(t *testing.T) {
	prefix := []string{"--settings", "a path/'\"$()`*\n\n", ""}
	prep := ""
	for i, arg := range prefix {
		prep += "printf %s " + shellQuote(arg) + " > \"$4/" + string(rune('0'+i)) + "\"\n"
	}
	prep += "printf 'terma-args-v1:3\\n' > \"$4/count\"\n"
	_, _, launcher := launcherFixture(t, prep)
	args := []string{"-p", "user\n", ""}
	got, stderr, err := runLauncher(t, launcher, args)
	if err != nil || stderr != "" || !reflect.DeepEqual(got, append(prefix, args...)) {
		t.Fatalf("args=%q stderr=%s err=%v", got, stderr, err)
	}
}

func TestLauncherBypassAndMaintenanceNeverPrepare(t *testing.T) {
	_, realDir, launcher := launcherFixture(t, "echo CALLED > \"$MARKER\"\nexit 1\n")
	marker := filepath.Join(realDir, "called")
	t.Setenv("MARKER", marker)
	for _, arg := range []string{"--help", "--version", "update", "login", "auth", "completion"} {
		got, stderr, err := runLauncher(t, launcher, []string{arg})
		if err != nil || stderr != "" || !reflect.DeepEqual(got, []string{arg}) {
			t.Fatalf("bypass %s: %q %s %v", arg, got, stderr, err)
		}
	}
	t.Setenv("TERMA_DISABLE", "1")
	_, stderr, err := runLauncher(t, launcher, []string{"-p", "hi"})
	if err != nil || stderr != "" {
		t.Fatalf("disable: %s %v", stderr, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("Terma ran on bypass path")
	}
}

func TestLauncherReResolvesPATHAndSkipsAliases(t *testing.T) {
	bin, realDir, launcher := launcherFixture(t, "")
	alias := filepath.Join(t.TempDir(), "shimlink")
	if err := os.Symlink(bin, alias); err != nil {
		t.Fatal(err)
	}
	replacement := t.TempDir()
	if err := os.WriteFile(filepath.Join(replacement, AgentClaude), []byte("#!/bin/sh\nprintf 'replacement\\000'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{alias + ":" + bin + "/../bin:" + replacement + ":" + realDir, bin + ":" + replacement} {
		t.Setenv("PATH", path)
		got, _, err := runLauncher(t, launcher, nil)
		if err != nil || !reflect.DeepEqual(got, []string{"replacement"}) {
			t.Fatalf("replacement: %q %v", got, err)
		}
	}
	t.Setenv("PATH", alias+":"+bin)
	_, _, err := runLauncher(t, launcher, nil)
	if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 127 {
		t.Fatalf("missing agent: %v", err)
	}
}

func TestLauncherPreservesIOAndNonzeroExitExactlyOnce(t *testing.T) {
	_, realDir, launcher := launcherFixture(t, "printf 'terma-args-v1:0\\n' > \"$4/count\"\n")
	marker := filepath.Join(realDir, "count")
	t.Setenv("MARKER", marker)
	if err := os.WriteFile(filepath.Join(realDir, AgentClaude), []byte("#!/bin/sh\necho x >> \"$MARKER\"\n/bin/cat\necho agent-stderr >&2\nexit 42\n"), 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(launcher)
	cmd.Stdin = strings.NewReader("original stdin\n")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 42 {
		t.Fatalf("exit: %v", err)
	}
	data, _ := os.ReadFile(marker)
	if string(data) != "x\n" || string(out) != "original stdin\n" || stderr.String() != "agent-stderr\n" {
		t.Fatalf("count=%q stdout=%q stderr=%q", data, out, stderr.String())
	}
}

func TestLauncherCancellationDuringPreparationDoesNotStartAgent(t *testing.T) {
	_, realDir, launcher := launcherFixture(t, "echo ready > \"$MARKER\"\nexec /bin/sleep 60\n")
	marker := filepath.Join(realDir, "ready")
	t.Setenv("MARKER", marker)
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	cmd := exec.Command(launcher)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("preparation did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 143 {
		t.Fatalf("cancellation: %v", err)
	}
	files, _ := os.ReadDir(temp)
	if len(files) != 0 {
		t.Fatal("preparation directory survived cancellation")
	}
}

func TestLauncherExecPreservesAgentPIDAndSignals(t *testing.T) {
	_, realDir, launcher := launcherFixture(t, "printf 'terma-args-v1:0\\n' > \"$4/count\"\n")
	marker := filepath.Join(realDir, "pid")
	t.Setenv("MARKER", marker)
	if err := os.WriteFile(filepath.Join(realDir, AgentClaude), []byte("#!/bin/sh\necho $$ > \"$MARKER\"\nexec /bin/sleep 60\n"), 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(launcher)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(marker)
		if err == nil {
			if strings.TrimSpace(string(data)) != strconv.Itoa(cmd.Process.Pid) {
				t.Fatal("agent was not execed")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	e, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("signal: %v", err)
	}
	status, ok := e.Sys().(syscall.WaitStatus)
	if !ok || status.Signal() != syscall.SIGTERM {
		t.Fatalf("signal status: %v", err)
	}
}

func TestShellFunctionsUseLauncherAndSurviveItsRemoval(t *testing.T) {
	bin, realDir, _ := launcherFixture(t, "printf 'terma-args-v1:0\\n' > \"$4/count\"\n")
	// Wrapper activation does not require putting the shim directory on PATH.
	t.Setenv("PATH", realDir)
	snippet := WrapperSnippet([]string{AgentClaude})
	for _, removed := range []bool{false, true} {
		if removed {
			if err := os.Remove(filepath.Join(bin, AgentClaude)); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("/bin/sh", "-c", snippet+"\nclaude \"$@\"", "test", "spaces and quotes '\"", "", "newline\n")
		out, err := cmd.Output()
		if err != nil || string(out) != "spaces and quotes '\"\x00\x00newline\n\x00" {
			t.Fatalf("removed=%v output=%q err=%v", removed, out, err)
		}
	}
}
