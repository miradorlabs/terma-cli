package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The profile's out-of-band captures that change how a build behaves, each a scenario of its
// own so the plain turn (TestClaudeProfile, TestCodexProfile) stays as a user's: egress through
// the recording proxy (Test…ProfileEgress), and the build's own telemetry export, timed, with
// its first model call held so each exporter's period passes (Test…ProfileTelemetry). Each
// writes its own profile row, beside the plain turn's, for the same build. What changes nothing
// the build sends (the files it writes, its hooks' payloads, its executable, its help) is taken
// on the plain turn itself.

// telemetryHold is how long a telemetry profile holds the first model call: past the
// one-minute metric export Claude Code and the OpenTelemetry SDKs default to.
const telemetryHold = 65 * time.Second

func isClaudeModelCall(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/messages") && !strings.Contains(r.URL.Path, "count_tokens")
}

func isCodexModelCall(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/responses")
}

// claudeProjectDir is the folder Claude Code keeps a working directory's transcripts in.
func claudeProjectDir(dir string) string {
	return regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(dir, "-")
}

// storageReplace are the sandbox's literals a stored path or key may hold: its paths, and the
// folder name each harness makes of the working directory, whole (Claude Code's, every
// character not a letter or digit a dash; Pi's and DeepSeek's, each slash) or from the test's
// own temporary folder on (omp's).
func (sb *Sandbox) storageReplace(session string) map[string]string {
	work := sb.workDir()
	tail := filepath.Join(filepath.Base(filepath.Dir(sb.Dir)), filepath.Base(sb.Dir), strings.TrimPrefix(work, sb.Dir))
	r := map[string]string{sb.Repo: "<repo>", sb.Dir: "<sandbox>"}
	for _, dir := range []string{work, tail} {
		r[claudeProjectDir(dir)] = "<project>"
		r[strings.ReplaceAll(dir, "/", "-")] = "<project>"
	}
	if session != "" {
		r[session] = "<session>"
	}
	return r
}

// claudeStorage is where Claude Code writes, and what of it is the sandbox's or terma's.
func (sb *Sandbox) claudeStorage(session string) StorageOptions {
	return StorageOptions{
		Roots:   []StorageRoot{{sb.ClaudeConfig, "<claude-config>"}, {sb.Home, "<home>"}, {sb.Repo, "<repo>"}},
		Replace: sb.storageReplace(session),
		Exclude: []string{"<claude-config>/settings.json"},
		Kinds: []StorageKind{
			{regexp.MustCompile(`^<claude-config>/projects/.*\.jsonl$`), "transcript"},
			{regexp.MustCompile(`^<claude-config>/\.claude\.json`), "state"},
		},
	}
}

// codexStorage is where Codex writes, and what of it is the sandbox's or terma's.
func (sb *Sandbox) codexStorage(thread string) StorageOptions {
	return StorageOptions{
		Roots:   []StorageRoot{{sb.CodexHome, "<codex-home>"}, {sb.Home, "<home>"}, {sb.Repo, "<repo>"}},
		Replace: sb.storageReplace(thread),
		Exclude: []string{"<codex-home>/config.toml", "<codex-home>/hooks.json", "<codex-home>/auth.json"},
		Kinds: []StorageKind{
			{regexp.MustCompile(`^<codex-home>/(archived_)?sessions/.*\.jsonl$`), "transcript"},
		},
	}
}

// codexThread is the thread `codex exec --json` printed it started.
func codexThread(out []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var ev map[string]any
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev["type"] == "thread.started" {
			id, _ := ev["thread_id"].(string)
			return id
		}
	}
	return ""
}

// proxyEnvNames are the variables StartEgressProxy's Env sets that route traffic.
var proxyEnvNames = []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NODE_USE_ENV_PROXY"}

// unproxyHooks has the recording shim run terma without the proxy, so what terma's hooks
// deliver is never taken for the build's egress.
func (sb *Sandbox) unproxyHooks() {
	t := sb.T
	t.Helper()
	path := filepath.Join(strings.TrimSuffix(sb.binDir(), ":"), "terma")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	shebang, rest, _ := strings.Cut(string(data), "\n")
	shim := shebang + "\nunset " + strings.Join(proxyEnvNames, " ") + "\n" + rest
	if err := os.WriteFile(path, []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
}

// egressSettle is how long a run's egress is awaited after the build exits: what it started
// in the background, or left to its exit.
const egressSettle = 3 * time.Second

// TestClaudeProfileEgress records everything each Claude Code build contacts besides its
// provider on the profile's turn, through the recording proxy: its defaults, nothing turned off.
func TestClaudeProfileEgress(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b))
		sb.unproxyHooks()
		proxy := StartEgressProxy(t)
		var calls atomic.Int32
		provider := httptest.NewServer(proxy.Provider(isClaudeModelCall, claudeWorkloadProvider(&calls, nil, false)))
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
		cmd.Env = append(sb.claudeEnv(RouteAPIKey), proxy.Env()...)
		out, err := cmd.CombinedOutput()
		time.Sleep(egressSettle)
		proxy.Record(t, b)
		if err != nil {
			t.Fatalf("claude -p behind the recording proxy: %v\n%s", err, out)
		}
		if calls.Load() == 0 {
			t.Fatal("Claude Code sent its provider no message request")
		}
	})
}

// TestCodexProfileEgress is the same for each Codex build's `codex exec`.
func TestCodexProfileEgress(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithCodex(b))
		sb.prepareCodex(RouteAPIKey)
		sb.unproxyHooks()
		proxy := StartEgressProxy(t)
		var calls atomic.Int32
		provider := httptest.NewServer(proxy.Provider(isCodexModelCall, codexWorkloadProvider(&calls, nil)))
		defer provider.Close()
		PlaceSandbox(t, b, sb)
		Placeholder(t, b, provider.URL, "<provider>")
		Placeholder(t, b, profilePrompt, "<prompt>")
		args := codexProfileArgs(sb, provider.URL)
		RecordCommand(t, b, args)
		ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, b.Path, append(args, profilePrompt)...)
		cmd.Dir = sb.workDir()
		cmd.Env = append(sb.codexEnv(RouteAPIKey), proxy.Env()...)
		out, err := cmd.CombinedOutput()
		time.Sleep(egressSettle)
		proxy.Record(t, b)
		if err != nil {
			t.Fatalf("codex exec behind the recording proxy: %v\n%s", err, out)
		}
		if calls.Load() == 0 {
			t.Fatal("Codex sent its provider no responses request")
		}
	})
}

// codexProfileArgs are TestCodexProfile's: the fixture provider and plugins off, nothing else.
func codexProfileArgs(sb *Sandbox, provider string) []string {
	return []string{"exec", "--json", "--skip-git-repo-check", "--dangerously-bypass-hook-trust",
		"-C", sb.workDir(), "-c", `cli_auth_credentials_store="file"`,
		"-c", "features.plugins=false", "-c", "features.remote_plugin=false",
		"-c", `model_provider="telemetry_fixture"`, "-c", `model_providers.telemetry_fixture.name="Telemetry fixture"`,
		"-c", "model_providers.telemetry_fixture.base_url=" + tomlQuote(provider),
		"-c", `model_providers.telemetry_fixture.wire_api="responses"`,
		"-c", `model_providers.telemetry_fixture.requires_openai_auth=false`}
}

// repointExporter has the exporter config at path send to url in place of from.
func repointExporter(t *testing.T, path, from, url string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(from)) {
		t.Fatalf("%s does not export to %s", path, from)
	}
	if err := os.WriteFile(path, bytes.ReplaceAll(data, []byte(from), []byte(url)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestClaudeProfileTelemetry records how each Claude Code build exports its own telemetry, with
// the exporter the field census's direct runs use (directClaude): every metric's type, unit and
// temporality, every span's parent, each signal's protocol and period. The period is the
// build's default: nothing sets it.
func TestClaudeProfileTelemetry(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b))
		front := TimeReceiver(t, sb.Receiver)
		sb.directClaude()
		repointExporter(t, filepath.Join(sb.ClaudeConfig, "settings.json"), sb.Receiver.URL(), front.URL)
		var hold HoldFirst
		var calls atomic.Int32
		provider := httptest.NewServer(hold.Wrap(telemetryHold, isClaudeModelCall, claudeWorkloadProvider(&calls, nil, false)))
		defer provider.Close()
		sb.ClaudeBaseURL = provider.URL
		PlaceSandbox(t, b, sb)
		Placeholder(t, b, provider.URL, "<provider>")
		Placeholder(t, b, front.URL, "<receiver>")
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
		time.Sleep(egressSettle)
		RecordTelemetry(t, b, front, hold.Answered())
	})
}

// TestCodexProfileTelemetry is the same for each Codex build, with directCodex's exporter.
func TestCodexProfileTelemetry(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithCodex(b))
		sb.prepareCodex(RouteAPIKey)
		front := TimeReceiver(t, sb.Receiver)
		repointExporter(t, filepath.Join(sb.CodexHome, "config.toml"), sb.Receiver.URL(), front.URL)
		var hold HoldFirst
		var calls atomic.Int32
		provider := httptest.NewServer(hold.Wrap(telemetryHold, isCodexModelCall, codexWorkloadProvider(&calls, nil)))
		defer provider.Close()
		PlaceSandbox(t, b, sb)
		Placeholder(t, b, provider.URL, "<provider>")
		Placeholder(t, b, front.URL, "<receiver>")
		args := codexProfileArgs(sb, provider.URL)
		RecordCommand(t, b, args)
		ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, b.Path, append(args, profilePrompt)...)
		cmd.Dir = sb.workDir()
		cmd.Env = sb.codexEnv(RouteAPIKey)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("codex exec: %v\n%s", err, out)
		}
		time.Sleep(egressSettle)
		RecordTelemetry(t, b, front, hold.Answered())
	})
}
