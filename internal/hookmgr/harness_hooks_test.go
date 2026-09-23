package hookmgr

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Every harness hook entry terma commits runs the same guarded command, and on a
// machine without terma it must be silent on both streams and exit 0. Claude Code
// prints a hook's stderr in the transcript on a non-zero exit and hands SessionStart
// and PostToolUse stderr to the model; SessionStart's stdout becomes context as well.
// The harnesses run the command with `sh -c` and JSON on stdin, which is how it is
// run here.
func TestHarnessHookCommandsAreInertWithoutTerma(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not installed")
	}
	var commands []string
	for _, h := range ClaudeHooks {
		commands = append(commands, h.Command)
	}
	for _, h := range CursorHooks {
		commands = append(commands, h.Command)
	}
	for _, h := range CodexHooks {
		commands = append(commands, h.Command)
	}
	for _, h := range AntigravityHooks {
		commands = append(commands, h.Command)
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			cmd := exec.Command("sh", "-c", command)
			cmd.Dir = t.TempDir()
			cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + cmd.Dir}
			cmd.Stdin = strings.NewReader(`{"session_id":"s1","hook_event_name":"SessionStart","cwd":"` + cmd.Dir + `"}`)
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("exit without terma: %v\nstderr: %s", err, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout without terma (the harness would show or feed this): %q", stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr without terma: %q", stderr.String())
			}
		})
	}
}

// A repository wired by an older terma carries the unguarded commands. Re-running
// `terma install` rewrites terma's own entries where they stand, leaves the user's
// entries alone, and is idempotent afterwards.
func TestHarnessHookEntriesUpgradeInPlace(t *testing.T) {
	root := t.TempDir()
	write(t, root, ClaudeSettingsPath, `{
  "hooks": {
    "SessionStart": [{"hooks": [{"type": "command", "command": "terma hook session-start", "timeout": 10}]}],
    "SessionEnd": [{"hooks": [{"type": "command", "command": "terma hook session-end", "timeout": 10}]}],
    "PostToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "./lint.sh"}]},
      {"matcher": "Edit|Write|MultiEdit|NotebookEdit", "hooks": [{"type": "command", "command": "terma hook post-tool-use", "timeout": 10}]}
    ]
  }
}
`)
	write(t, root, CursorHooksPath, `{
  "version": 1,
  "hooks": {
    "sessionStart": [{"command": "terma hook cursor-session-start", "timeout": 10}],
    "sessionEnd": [{"command": "terma hook cursor-session-end", "timeout": 10}],
    "afterFileEdit": [{"command": "./format.sh"}, {"command": "terma hook cursor-file-edit", "timeout": 10}]
  }
}
`)
	write(t, root, CodexHooksPath, `{
  "hooks": {
    "SessionStart": [{"hooks": [{"type": "command", "command": "terma hook codex-session-start", "timeout": 10}]}],
    "PostToolUse": [
      {"matcher": "^shell$", "hooks": [{"type": "command", "command": "./audit.sh"}]},
      {"hooks": [{"type": "command", "command": "terma hook codex-post-tool-use", "timeout": 10, "async": true}]}
    ],
    "SessionEnd": [{"hooks": [{"type": "command", "command": "terma hook codex-session-end", "timeout": 3}]}]
  }
}
`)
	write(t, root, AntigravityHooksPath, `{
  "format": {
    "PostToolUse": [{"matcher": "write_to_file", "hooks": [{"type": "command", "command": "./format.sh"}]}]
  },
  "terma": {
    "PostToolUse": [{"matcher": "", "hooks": [{"type": "command", "command": "terma hook antigravity-post-tool-use", "timeout": 10}]}],
    "Stop": [{"type": "command", "command": "terma hook antigravity-stop", "timeout": 10}]
  }
}
`)

	plans := map[string]func(string, bool) (Plan, error){
		ClaudeSettingsPath:   PlanClaudeSettings,
		CursorHooksPath:      PlanCursorHooks,
		CodexHooksPath:       PlanCodexHooks,
		AntigravityHooksPath: PlanAntigravityHooks,
	}
	// How many terma entries each file carries once upgraded: the three the
	// stale files above had, plus the newer turn and failure hooks.
	guarded := map[string]int{ClaudeSettingsPath: len(ClaudeHooks), CursorHooksPath: len(CursorHooks), CodexHooksPath: len(CodexHooks), AntigravityHooksPath: len(AntigravityHooks)}
	for path, plan := range plans {
		p, err := plan(root, true)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if p.Empty() {
			t.Fatalf("%s: stale commands were not planned for an upgrade", path)
		}
		if err := Apply(root, p); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		got := read(t, root, path)
		if strings.Contains(got, `"terma hook`) {
			t.Fatalf("%s still carries an unguarded command:\n%s", path, got)
		}
		if n := strings.Count(got, HookCommand("")[:len("command -v terma")]); n != guarded[path] {
			t.Fatalf("%s should carry exactly %d guarded commands, has %d:\n%s", path, guarded[path], n, got)
		}
		if again, _ := plan(root, true); !again.Empty() {
			t.Fatalf("%s: install should be idempotent after the upgrade: %+v", path, again.Changes)
		}
	}
	for path, user := range map[string]string{
		ClaudeSettingsPath:   "./lint.sh",
		CursorHooksPath:      "./format.sh",
		CodexHooksPath:       "./audit.sh",
		AntigravityHooksPath: "./format.sh",
	} {
		if !strings.Contains(read(t, root, path), user) {
			t.Fatalf("%s: the user's own hook %s was lost", path, user)
		}
	}
}

// The committed files hold the guard as written. encoding/json would otherwise commit
// `>` and `&` as \u003e and \u0026 — and rewrite any user hook that carries a redirect
// the same way.
func TestHookFilesAreNotHTMLEscaped(t *testing.T) {
	root := t.TempDir()
	const userHook = `echo edited >> hooks.log && true`
	write(t, root, ClaudeSettingsPath, `{
  "hooks": {
    "PostToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "`+userHook+`"}]}]
  }
}
`)
	plan, err := PlanClaudeSettings(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	got := read(t, root, ClaudeSettingsPath)
	if strings.Contains(got, `\u00`) {
		t.Fatalf("HTML-escaped characters in a committed file:\n%s", got)
	}
	if !strings.Contains(got, HookCommand("session-start")) {
		t.Fatalf("guarded command not written verbatim:\n%s", got)
	}
	if !strings.Contains(got, userHook) {
		t.Fatalf("user's redirecting hook was rewritten:\n%s", got)
	}
	// Still valid JSON that parses back to the same commands.
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("%v:\n%s", err, got)
	}
	if doc.Hooks["PostToolUse"][0].Hooks[0].Command != userHook {
		t.Fatalf("user hook parsed as %q", doc.Hooks["PostToolUse"][0].Hooks[0].Command)
	}
	if doc.Hooks["SessionStart"][0].Hooks[0].Command != HookCommand("session-start") {
		t.Fatalf("terma hook parsed as %q", doc.Hooks["SessionStart"][0].Hooks[0].Command)
	}
	_ = os.Remove // keep os imported for readers extending this test with file checks
}
