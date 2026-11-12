package e2e

import (
	"context"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// profilePrompt is the profile scenarios' one turn; the fixture provider replies whatever it
// says, and normalization replaces it wherever a request repeats it.
const profilePrompt = "Say hello. TERMA_PROFILE"

// TestClaudeProfile records what each Claude Code build sends its provider on a first turn of
// `claude -p` as a user runs it: its own system prompt, every tool it offers, its default
// model, betas and cache breakpoints. None of the suite's settings (claudeArgs pins a system
// prompt, a model and turns off slash commands and MCP) is passed. After the turn it records
// the files the build wrote, its hooks' payloads, its executable's facts and its help.
func TestClaudeProfile(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b))
		var calls atomic.Int32
		provider := httptest.NewServer(CaptureProvider(t, b, claudeWorkloadProvider(&calls, nil, false)))
		defer provider.Close()
		sb.ClaudeBaseURL = provider.URL
		PlaceSandbox(t, b, sb)
		Placeholder(t, b, provider.URL, "<provider>")
		Placeholder(t, b, profilePrompt, "<prompt>")
		session := uuid.NewString()
		args := []string{"--output-format", "json", "--max-turns", "1", "--session-id", session}
		RecordCommand(t, b, args)
		storage := sb.claudeStorage(session)
		before := SnapshotFiles(storage.Roots...)
		ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, sb.claudeLauncher(), append([]string{"-p", profilePrompt}, args...)...)
		cmd.Dir = sb.workDir()
		cmd.Env = sb.claudeEnv(RouteAPIKey)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("claude -p: %v\n%s", err, out)
		}
		if calls.Load() == 0 {
			t.Fatal("Claude Code sent its provider no message request")
		}
		RecordStorage(t, b, before, storage)
		RecordHooks(t, b, sb)
		RecordBinaryFacts(t, b)
		RecordHelpTree(t, b, sb.claudeEnv(RouteAPIKey), "claude")
		RecordSigning(t, b)
	})
}

// TestClaudeProfileBinary records only what each Claude Code build's executable says of
// itself and who signed it, with no turn: a backfill re-reads many builds' facts (their
// settings schema, the variables they read) in the time a few turns take. refgen prefers its
// facts to an earlier scenario's.
func TestClaudeProfileBinary(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		RecordBinaryFacts(t, b)
		RecordSigning(t, b)
	})
}

// TestCodexProfile is the same for each Codex build's `codex exec`, with its defaults: the
// sandbox, approval policy and reasoning effort a user's first run has (CodexExec pins the
// last). Only the fixture provider and plugins (whose catalog clone outlives Codex) are set.
// It records the same out-of-band evidence after the turn.
func TestCodexProfile(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithCodex(b))
		sb.prepareCodex(RouteAPIKey)
		var calls atomic.Int32
		provider := httptest.NewServer(CaptureProvider(t, b, codexWorkloadProvider(&calls, nil)))
		defer provider.Close()
		PlaceSandbox(t, b, sb)
		Placeholder(t, b, provider.URL, "<provider>")
		Placeholder(t, b, profilePrompt, "<prompt>")
		args := codexProfileArgs(b, sb, provider.URL)
		RecordCommand(t, b, args)
		before := SnapshotFiles(sb.codexStorage("").Roots...)
		ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, b.Path, append(args, profilePrompt)...)
		cmd.Dir = sb.workDir()
		cmd.Env = sb.codexEnv(RouteAPIKey)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("codex exec: %v\n%s", err, out)
		}
		if calls.Load() == 0 {
			t.Fatal("Codex sent its provider no responses request")
		}
		RecordStorage(t, b, before, sb.codexStorage(codexThread(out)))
		RecordHooks(t, b, sb)
		RecordBinaryFacts(t, b)
		RecordHelpTree(t, b, sb.codexEnv(RouteAPIKey), "codex")
		RecordSigning(t, b)
	})
}

// codexExecHelp is what b's `codex exec --help` prints, "" where it prints nothing.
func codexExecHelp(b Binary, env []string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.Path, "exec", "--help")
	cmd.Env = env
	out, _ := cmd.CombinedOutput()
	return string(out)
}

var (
	codexFlagsMu sync.Mutex
	codexFlags   = map[string][]string{}
)

// codexOptionalFlags are the flags the scenarios pass a `codex exec` that an older build may
// not have, those sb's build lists in its help: one it does not know would end its run before
// its first request.
func codexOptionalFlags(sb *Sandbox) []string {
	codexFlagsMu.Lock()
	defer codexFlagsMu.Unlock()
	if flags, ok := codexFlags[sb.Codex.Path]; ok {
		return flags
	}
	help := codexExecHelp(sb.Codex, sb.codexEnv(RouteAPIKey))
	flags := []string{}
	for _, flag := range []string{"--skip-git-repo-check", "--dangerously-bypass-hook-trust"} {
		if strings.Contains(help, flag) {
			flags = append(flags, flag)
		}
	}
	codexFlags[sb.Codex.Path] = flags
	return flags
}
