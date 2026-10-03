package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Claude Desktop's Code tab runs Claude Code through the Agent SDK, never through the
// `claude` on PATH (Desktop 1.19367.0, read from its app bundle, 2026-09-29):
//
//   - its own build, pinned by the app (2.1.202 in that release), under
//     ~/Library/Application Support/Claude/claude-code/<version>/;
//   - started through Contents/Helpers/disclaimer, which stays alive as the parent
//     (Claude → disclaimer → claude);
//   - `--input-format stream-json --output-format stream-json` on pipes, no terminal,
//     `--setting-sources=user,project,local` — so the user's settings.json env, where
//     `terma relay setup` puts the exporter, and the repository's hooks both apply;
//   - CLAUDE_CODE_ENTRYPOINT=claude-desktop, and OTEL_SERVICE_NAME /
//     OTEL_RESOURCE_ATTRIBUTES naming the service claude-code-desktop.
//
// ClaudeDesktopRun reproduces that launch. What it cannot reproduce is Desktop's own
// OAuth token: the fake provider stands in for Anthropic, as in every other scenario.

// claudeDesktopDir is where Desktop keeps the Claude Code builds it runs.
func claudeDesktopDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Application Support", "Claude", "claude-code")
}

// ClaudeDesktopBinaries are the builds a Desktop session runs: those Desktop has
// installed on this machine (else the one its current release pins), then the newest
// Claude Code, so a Desktop release that moves its pin forward is already covered.
// TERMA_E2E_CLAUDE_DESKTOP_VERSIONS overrides the pinned set.
func ClaudeDesktopBinaries(t *testing.T) []Binary {
	t.Helper()
	spec := os.Getenv("TERMA_E2E_CLAUDE_DESKTOP_VERSIONS")
	if spec == "" {
		var pinned []string
		entries, _ := os.ReadDir(claudeDesktopDir())
		for _, e := range entries {
			if e.IsDir() && numberOf(e.Name()) == e.Name() {
				pinned = append(pinned, e.Name())
			}
		}
		if len(pinned) == 0 {
			pinned = []string{"2.1.202"}
		}
		spec = strings.Join(pinned, ",")
	}
	builds := resolve(t, "claude", spec, claudeVersionList, ensureClaude)
	if newest := ClaudeBinaries(t); len(newest) > 0 {
		n := newest[len(newest)-1]
		if !slices.ContainsFunc(builds, func(b Binary) bool { return b.Version == n.Version }) {
			builds = append(builds, n)
		}
	}
	return builds
}

func forEachClaudeDesktop(t *testing.T, run func(t *testing.T, b Binary)) {
	t.Helper()
	if !Enabled() {
		t.Skip("live tests run only with TERMA_E2E=1")
	}
	for _, b := range ClaudeDesktopBinaries(t) {
		t.Run(b.Label(), func(t *testing.T) { run(t, b) })
	}
}

// desktopParent is what Desktop starts Claude Code under: its disclaimer helper when
// the app is installed, else a shell that stays the parent the same way.
func desktopParent() []string {
	const disclaimer = "/Applications/Claude.app/Contents/Helpers/disclaimer"
	if _, err := os.Stat(disclaimer); err == nil {
		return []string{disclaimer}
	}
	return []string{"/bin/sh", "-c", `"$@"; exit $?`, "sh"}
}

// ClaudeDesktopRun runs one Desktop-shaped Code-tab turn with the sandbox's Claude in
// dir and returns the session id Claude Code reported.
func (sb *Sandbox) ClaudeDesktopRun(dir, prompt string, extra ...string) string {
	t := sb.T
	t.Helper()
	sb.directClaude()
	args := append(desktopParent(), sb.Claude.Path,
		"--output-format", "stream-json", "--verbose", "--input-format", "stream-json",
		"--model", "claude-haiku-4-5", "--permission-prompt-tool", "stdio",
		"--setting-sources=user,project,local",
		"--permission-mode", "bypassPermissions", "--allow-dangerously-skip-permissions",
		"--include-partial-messages", "--settings", "{}", "--replay-user-messages", "--max-turns", "12")
	args = append(args, extra...)
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = dir
	var env []string
	for _, kv := range sb.claudeEnv(RouteAPIKey) {
		if !strings.HasPrefix(kv, "CLAUDECODE=") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env,
		"CLAUDE_CODE_ENTRYPOINT=claude-desktop",
		"CLAUDE_AGENT_SDK_VERSION=0.3.202",
		"DISABLE_AUTOUPDATER=1",
		"DISABLE_MICROCOMPACT=1",
		"MCP_CONNECTION_NONBLOCKING=true",
		"CLAUDE_CODE_EMIT_TOOL_USE_SUMMARIES=false",
		"OTEL_SERVICE_NAME=claude-code-desktop",
		"OTEL_RESOURCE_ATTRIBUTES=service.name=claude-code-desktop,service.version=1.19367.0,os.type=darwin")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	msg, _ := json.Marshal(map[string]any{"type": "user", "session_id": "", "parent_tool_use_id": nil,
		"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": prompt}}}})
	if _, err := stdin.Write(append(msg, '\n')); err != nil {
		t.Fatal(err)
	}
	// Desktop keeps stdin open for the next message; the turn ends at its result.
	var session string
	var transcript strings.Builder
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		transcript.Write(line)
		transcript.WriteByte('\n')
		var ev struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
			IsError   bool   `json:"is_error"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if session == "" && ev.SessionID != "" {
			session = ev.SessionID
		}
		if ev.Type == "result" {
			if ev.IsError {
				t.Errorf("the Desktop turn ended in an error:\n%s", tail(transcript.String(), 2000))
			}
			break
		}
	}
	_ = stdin.Close()
	_, _ = io.Copy(io.Discard, stdout)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Desktop-shaped claude: %v\n%s\n%s", err, tail(transcript.String(), 2000), stderr.String())
	}
	if session == "" {
		t.Fatalf("Claude Code reported no session:\n%s", tail(transcript.String(), 2000))
	}
	return session
}
