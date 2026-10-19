//go:build !windows

package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// `terma launch` becomes the agent's command: its arguments, streams and exit status are
// the command's own. It adds the relay exporter only while a relay is set up and setup
// last recorded the agent as chosen, and otherwise starts the command as it found it.
func TestLaunchStartsTheCommandWithItsExporter(t *testing.T) {
	terma := termaBinary(t)
	home, bin := t.TempDir(), t.TempDir()
	// A stand-in for the Copilot CLI: it prints its arguments and exporter environment.
	fake := "#!/bin/sh\nprintf 'args:'; printf ' [%s]' \"$@\"; echo\nenv | grep -E '^(COPILOT_OTEL|OTEL_)' | sort\nexit 7\n"
	if err := os.WriteFile(filepath.Join(bin, "copilot"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	configDir, state := filepath.Join(home, "config"), filepath.Join(home, "state")
	var extra []string
	run := func(path string, args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(terma, args...)
		cmd.Env = append([]string{"HOME=" + home, "PATH=" + path, "TERMA_ENV=dev",
			"TERMA_CONFIG_DIR=" + configDir, "TERMA_STATE_DIR=" + state,
			// The developer's own exporter settings, for a collector of theirs.
			"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://mine.example.com/v1/traces", "OTEL_SERVICE_NAME=mine"}, extra...)
		out, err := cmd.CombinedOutput()
		code := 0
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return string(out), code
	}
	untouched := "args: [-p] [two words] [--model=x]\nOTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://mine.example.com/v1/traces\nOTEL_SERVICE_NAME=mine\n"

	// Before setup: the command runs as the developer set it up.
	if out, code := run(bin+":/usr/bin:/bin", "launch", "copilot", "--", "-p", "two words", "--model=x"); out != untouched || code != 7 {
		t.Fatalf("before setup: exit %d\n%s", code, out)
	}

	// Setup: a relay token, and the agent among the ones the developer chose.
	if err := os.MkdirAll(filepath.Dir(claim.TokenPath(state)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claim.TokenPath(state), []byte("tok-0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	choose := func(agents ...string) {
		t.Helper()
		if err := config.UpdateProfile(configDir, config.DefaultProfile, func(p *config.Profile) { p.Harnesses = agents }); err != nil {
			t.Fatal(err)
		}
	}
	choose("claude", "copilot")
	out, code := run(bin+":/usr/bin:/bin", "launch", "copilot", "--", "-p", "two words", "--model=x")
	want := "args: [-p] [two words] [--model=x]\n" +
		"COPILOT_OTEL_ENABLED=true\n" +
		"OTEL_EXPORTER_OTLP_ENDPOINT=http://" + claim.DefaultAddr + "/tok-0123456789abcdef\n" +
		"OTEL_EXPORTER_OTLP_PROTOCOL=http/json\n" +
		"OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=true\n" +
		"OTEL_SERVICE_NAME=mine\n"
	if out != want || code != 7 {
		t.Fatalf("after setup: exit %d\n%s\nwant:\n%s", code, out, want)
	}
	// With no arguments at all (printf then prints its format once), and with a flag where
	// terma's own would go.
	if out, _ := run(bin+":/usr/bin:/bin", "launch", "copilot"); !strings.HasPrefix(out, "args: []\nCOPILOT_OTEL_ENABLED=true\n") {
		t.Fatalf("no arguments:\n%s", out)
	}
	if out, _ := run(bin+":/usr/bin:/bin", "launch", "copilot", "--help"); !strings.HasPrefix(out, "args: [--help]\n") {
		t.Fatalf("the command's own flag was read as terma's:\n%s", out)
	}

	// The kill switch, in the shell that starts it.
	extra = []string{"TERMA_SHELL_LAUNCHERS=0"}
	if out, code := run(bin+":/usr/bin:/bin", "launch", "copilot", "--", "-p", "two words", "--model=x"); out != untouched || code != 7 {
		t.Fatalf("under the kill switch: exit %d\n%s", code, out)
	}
	extra = nil

	// Setup no longer records the agent: a shell still holding the function starts it bare.
	choose("claude")
	if out, code := run(bin+":/usr/bin:/bin", "launch", "copilot", "--", "-p", "two words", "--model=x"); out != untouched || code != 7 {
		t.Fatalf("after setup dropped the agent: exit %d\n%s", code, out)
	}

	// The command is not installed, or is not one terma starts.
	if out, code := run(t.TempDir()+":/usr/bin:/bin", "launch", "copilot"); code != 127 || out != "copilot: command not found\n" {
		t.Fatalf("a missing command: exit %d\n%s", code, out)
	}
	if out, code := run(bin+":/usr/bin:/bin", "launch", "claude"); code != 1 || !strings.Contains(out, "not a command terma starts") {
		t.Fatalf("an agent terma does not start: exit %d\n%s", code, out)
	}
}
