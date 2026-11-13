package e2e

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Profiles of the other harnesses with a fixture provider: each build's first turn as a user
// runs it headless, against the fixture its relay scenarios use, with nothing of terma's
// installed (no extension, plugin or exporter: setup records only Claude Code and Codex). What
// a run must set that a user's would not, the fixture as its only model above all, is in the
// recorded command or the comment.

// harnessProfile runs one profile turn of b: provider (wrapped in CaptureProvider) is served,
// configure points the sandbox at its URL, and args (the prompt last) are run in the sandbox
// repository with env, bounded by scenarioTimeout. The help of each of helps is recorded.
func harnessProfile(t *testing.T, b Binary, provider http.Handler, configure func(sb *Sandbox, url string) []string,
	args func(sb *Sandbox) []string, helps ...[]string) *Sandbox {
	t.Helper()
	track(t)
	sb := New(t, Isolated)
	srv := httptest.NewServer(CaptureProvider(t, b, provider))
	t.Cleanup(srv.Close)
	extra := configure(sb, srv.URL) // first: it may add to the sandbox's own environment
	env := append(sb.termaEnv(), extra...)
	PlaceSandbox(t, b, sb)
	placeUnresolved(t, b, sb)
	Placeholder(t, b, srv.URL, "<provider>")
	Placeholder(t, b, profilePrompt, "<prompt>")
	a := args(sb)
	RecordCommand(t, b, a[:len(a)-1])
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.Path, a...)
	cmd.Dir = sb.Repo
	cmd.Env = env
	cmd.Stdin = nil
	cmd.WaitDelay = 5 * time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s: %v\n%s", b.Harness, err, tail(out.String(), 3000))
	}
	for _, h := range helps {
		RecordHelp(t, b, env, h...)
	}
	return sb
}

// requireCalls fails t if the build sent the fixture no model request.
func requireCalls(t *testing.T, b Binary, calls *atomic.Int32) {
	t.Helper()
	if calls.Load() == 0 {
		t.Fatalf("%s sent its provider no model request", b.Harness)
	}
}

// TestGeminiProfile: `gemini -p` with its default model and approval mode, signed in with an
// API key against the fake Gemini API.
func TestGeminiProfile(t *testing.T) {
	forEachGemini(t, func(t *testing.T, b Binary) {
		var calls atomic.Int32
		harnessProfile(t, b, geminiProvider(&calls, "", nil), func(sb *Sandbox, url string) []string {
			sb.UseGemini(url)
			return nil
		}, func(*Sandbox) []string {
			return []string{"--output-format", "json", "-p", profilePrompt}
		}, []string{"--help"}, []string{"mcp", "--help"}, []string{"extensions", "--help"})
		requireCalls(t, b, &calls)
	})
}

// TestOpenCodeProfile: `opencode run`, its only model the fixture's (an OpenAI-compatible
// provider, as every OpenCode fixture is), permissions as the relay scenarios set them.
func TestOpenCodeProfile(t *testing.T) {
	forEachOpenCode(t, func(t *testing.T, b Binary, _ bool) {
		var calls atomic.Int32
		harnessProfile(t, b, openAIChatProvider(&calls), func(sb *Sandbox, url string) []string {
			sb.UseOpenCodeProvider(url)
			return []string{"XDG_CONFIG_HOME=" + filepath.Join(sb.Home, ".config")}
		}, func(sb *Sandbox) []string {
			return []string{"run", "--format", "json", "--dir", sb.Repo, profilePrompt}
		}, []string{"--help"}, []string{"run", "--help"})
		requireCalls(t, b, &calls)
	})
}

// TestPiProfile: `pi -p` with the fixture as its model. --offline only stops Pi's startup
// network work (update and catalog checks); the request is what it sends anyway.
func TestPiProfile(t *testing.T) {
	forEachPi(t, func(t *testing.T, b Binary) {
		var calls atomic.Int32
		harnessProfile(t, b, openAIToolCallProvider(&calls, "", nil), func(sb *Sandbox, url string) []string {
			sb.UsePiProvider(url)
			return []string{"PI_CODING_AGENT_DIR=" + sb.piAgentDir()}
		}, func(*Sandbox) []string {
			return []string{"-p", "--model", "fake/m", "--offline", profilePrompt}
		}, []string{"--help"})
		requireCalls(t, b, &calls)
	})
}

// TestOmpProfile: `omp -p` with the fixture as its model.
func TestOmpProfile(t *testing.T) {
	forEachOmp(t, func(t *testing.T, b Binary) {
		var calls atomic.Int32
		harnessProfile(t, b, openAIToolProvider(&calls, ""), func(sb *Sandbox, url string) []string {
			sb.UseOmpProvider(url)
			return nil
		}, func(*Sandbox) []string {
			return []string{"-p", "--model", "fake/m", profilePrompt}
		}, []string{"--help"})
		requireCalls(t, b, &calls)
	})
}

// TestHermesProfile: `hermes chat -q`, quiet, with the fixture as its only model.
func TestHermesProfile(t *testing.T) {
	forEachHermes(t, func(t *testing.T, b Binary) {
		var calls atomic.Int32
		harnessProfile(t, b, hermesProvider(&calls, "", nil), func(sb *Sandbox, url string) []string {
			sb.UseHermes(url)
			return nil
		}, func(*Sandbox) []string {
			return []string{"chat", "-Q", "-q", profilePrompt}
		}, []string{"--help"}, []string{"chat", "--help"})
		requireCalls(t, b, &calls)
	})
}

// TestDshProfile: `dsh headless`, its native adapter pointed at the Anthropic Messages fixture
// that stands in for DeepSeek, and DeepSeek's own session telemetry off.
func TestDshProfile(t *testing.T) {
	forEachDsh(t, func(t *testing.T, b Binary) {
		var calls atomic.Int32
		harnessProfile(t, b, claudeProbeProvider(&calls, nil, nil), func(sb *Sandbox, url string) []string {
			sb.UseDsh(url)
			return nil
		}, func(*Sandbox) []string {
			return []string{"headless", profilePrompt}
		}, []string{"--help"}, []string{"headless", "--help"})
		requireCalls(t, b, &calls)
	})
}

// TestCopilotProfile: `copilot -p` with a custom provider (BYOK: COPILOT_PROVIDER_BASE_URL),
// the Anthropic Messages fixture, so no GitHub token is needed or used.
func TestCopilotProfile(t *testing.T) {
	forEachCopilotBuild(t, func(t *testing.T, b Binary) {
		var calls atomic.Int32
		harnessProfile(t, b, claudeProbeProvider(&calls, nil, nil), copilotFixtureEnv, func(*Sandbox) []string {
			return []string{"-p", profilePrompt}
		}, []string{"--help"}, []string{"help", "environment"}, []string{"help", "providers"})
		requireCalls(t, b, &calls)
	})
}

// forEachCopilotBuild is forEachCopilot for scenarios against a fixture provider: they need no
// COPILOT_GITHUB_TOKEN.
func forEachCopilotBuild(t *testing.T, run func(t *testing.T, b Binary)) {
	t.Helper()
	if !Enabled() {
		t.Skip("live tests run only with TERMA_E2E=1")
	}
	for _, b := range CopilotBinaries(t) {
		t.Run(b.Label(), func(t *testing.T) { scenarioOfBuild(t, b); run(t, b) })
	}
}

// copilotFixtureEnv is the environment that makes the fixture at url the Copilot CLI's model
// provider (BYOK, Anthropic's API), in a home of the sandbox's, with auto-update off so the build
// under test is the one that runs.
func copilotFixtureEnv(sb *Sandbox, url string) []string {
	return []string{"COPILOT_HOME=" + filepath.Join(sb.Home, ".copilot"), "COPILOT_AUTO_UPDATE=false",
		"COPILOT_PROVIDER_TYPE=anthropic", "COPILOT_PROVIDER_BASE_URL=" + url, "COPILOT_PROVIDER_API_KEY=synthetic",
		"COPILOT_MODEL=" + copilotModel}
}

// placeUnresolved records the placeholders of sb's paths as a build may also print them on
// macOS: through /var, the symlink the sandbox's /private/var paths are resolved from.
func placeUnresolved(t *testing.T, b Binary, sb *Sandbox) {
	t.Helper()
	for literal, name := range map[string]string{sb.Repo: "<repo>", sb.Home: "<home>", sb.Dir: "<sandbox>"} {
		if rest, ok := strings.CutPrefix(literal, "/private/var/"); ok {
			Placeholder(t, b, "/var/"+rest, name)
		}
	}
}

// copilotModel is the model a Copilot profile names: BYOK needs one, and a model Copilot's
// catalog knows gets the agent configuration (prompt, tools, limits) it has for it.
const copilotModel = "claude-sonnet-4.5"
