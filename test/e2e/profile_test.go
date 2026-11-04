package e2e

import (
	"context"
	"net/http/httptest"
	"os/exec"
	"strings"
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
// prompt, a model and turns off slash commands and MCP) is passed.
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
		args := []string{"--output-format", "json", "--max-turns", "1", "--session-id", uuid.NewString()}
		RecordCommand(t, b, args)
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
		RecordHelp(t, b, sb.claudeEnv(RouteAPIKey), "--help")
	})
}

// TestCodexProfile is the same for each Codex build's `codex exec`, with its defaults: the
// sandbox, approval policy and reasoning effort a user's first run has (CodexExec pins the
// last). Only the fixture provider and plugins (whose catalog clone outlives Codex) are set.
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
		// A flag an older build does not have would end its run before its first request:
		// each is passed where the build's own help lists it.
		args := []string{"exec", "--json"}
		help := codexExecHelp(b, sb.codexEnv(RouteAPIKey))
		for _, flag := range []string{"--skip-git-repo-check", "--dangerously-bypass-hook-trust"} {
			if strings.Contains(help, flag) {
				args = append(args, flag)
			}
		}
		args = append(args, "-C", sb.workDir(), "-c", `cli_auth_credentials_store="file"`,
			"-c", "features.plugins=false", "-c", "features.remote_plugin=false",
			"-c", `model_provider="telemetry_fixture"`, "-c", `model_providers.telemetry_fixture.name="Telemetry fixture"`,
			"-c", "model_providers.telemetry_fixture.base_url="+tomlQuote(provider.URL),
			"-c", `model_providers.telemetry_fixture.wire_api="responses"`,
			"-c", `model_providers.telemetry_fixture.requires_openai_auth=false`)
		RecordCommand(t, b, args)
		ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, b.Path, append(args, profilePrompt)...)
		cmd.Dir = sb.workDir()
		cmd.Env = sb.codexEnv(RouteAPIKey)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("codex exec: %v\n%s", err, out)
		}
		if calls.Load() == 0 {
			t.Fatal("Codex sent its provider no responses request")
		}
		RecordHelp(t, b, sb.codexEnv(RouteAPIKey), "--help")
		RecordHelp(t, b, sb.codexEnv(RouteAPIKey), "exec", "--help")
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
