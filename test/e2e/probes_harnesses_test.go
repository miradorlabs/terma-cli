package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The behaviour probes that cost little for the other harnesses with a fixture provider:
// across-turns, retries and tool-output, each as Claude Code's and Codex's are (probes.go,
// probes_test.go), from one driver a harness. A harness a probe cannot drive (dsh resumes only a
// session id its JSON events name) is left out of that probe.

// harnessDriver is how the probes run one harness: its builds, its fixture, and its command line.
type harnessDriver struct {
	harness string
	each    func(t *testing.T, run func(t *testing.T, b Binary))
	// provider is the build's fixture; where cmd is not empty, it first asks the build's shell tool
	// to run it.
	provider func(calls *atomic.Int32, cmd string) http.Handler
	// modelCall says whether a request is a model call, what a retries probe fails.
	modelCall func(r *http.Request) bool
	// failures are a retries probe's statuses, each with the error body the provider's API sends.
	failures []providerFailure
	// configure points sb at the fixture's url, and returns what the build's environment adds.
	configure func(sb *Sandbox, url string) []string
	// args is one turn's command line, the prompt last: resume continues the session session (the
	// build's last where the harness has no session flag), tools lets the shell tool run.
	args func(sb *Sandbox, prompt, session string, resume, tools bool) []string
	// resumes says whether across-turns can drive the harness.
	resumes bool
}

// providerFailure is one status a retries probe answers with, and its body.
type providerFailure struct {
	status int
	body   string
}

var (
	openAIFailures = []providerFailure{
		{429, `{"error":{"message":"Rate limit reached","type":"requests","code":"rate_limit_exceeded"}}`},
		{500, `{"error":{"message":"The server had an error","type":"server_error"}}`},
	}
	anthropicFailures = []providerFailure{
		{429, `{"type":"error","error":{"type":"rate_limit_error","message":"Rate limited"}}`},
		{529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`},
	}
	geminiFailures = []providerFailure{
		{429, `{"error":{"code":429,"message":"Resource has been exhausted","status":"RESOURCE_EXHAUSTED"}}`},
		{500, `{"error":{"code":500,"message":"Internal error encountered.","status":"INTERNAL"}}`},
	}
)

func chatCompletions(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/chat/completions") }
func anthropicMessages(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/v1/messages") && !strings.Contains(r.URL.Path, "count_tokens")
}
func geminiGenerate(r *http.Request) bool { return strings.Contains(r.URL.Path, "enerateContent") }

// shellStep is the one Messages API tool call of cmd in a shell tool named tool.
func shellStep(tool, cmd string) []claudeStep {
	if cmd == "" {
		return nil
	}
	return []claudeStep{{{"name": tool, "input": map[string]any{"command": cmd, "description": "probe"}}}}
}

// openAIBash is openAIProbeProvider asking for one call of a bash tool with args where cmd is not
// empty, and only replying where it is.
func openAIBash(calls *atomic.Int32, cmd string, args map[string]any) http.Handler {
	if cmd == "" {
		return openAIProbeProvider(calls, "", nil)
	}
	return openAIProbeProvider(calls, "bash", args)
}

// harnessDrivers are the other harnesses the probes run, as their profiles do
// (profile_harnesses_test.go).
var harnessDrivers = []harnessDriver{
	{
		harness: "gemini", each: forEachGemini, modelCall: geminiGenerate, failures: geminiFailures, resumes: true,
		provider: func(calls *atomic.Int32, cmd string) http.Handler {
			if cmd == "" {
				return geminiProvider(calls, "", nil)
			}
			return geminiProvider(calls, "run_shell_command", map[string]any{"command": cmd})
		},
		configure: func(sb *Sandbox, url string) []string { sb.UseGemini(url); return nil },
		args: func(_ *Sandbox, prompt, _ string, resume, tools bool) []string {
			a := []string{"--output-format", "json"}
			if resume {
				a = append(a, "--resume", "latest")
			}
			if tools {
				a = append(a, "--yolo") // headless Gemini offers no shell tool without it
			}
			return append(a, "-p", prompt)
		},
	},
	{
		harness: "opencode", each: func(t *testing.T, run func(*testing.T, Binary)) {
			forEachOpenCode(t, func(t *testing.T, b Binary, _ bool) { run(t, b) })
		},
		modelCall: chatCompletions, failures: openAIFailures, resumes: true,
		provider: func(calls *atomic.Int32, cmd string) http.Handler {
			return openAIBash(calls, cmd, map[string]any{"command": cmd, "description": "probe"})
		},
		configure: func(sb *Sandbox, url string) []string {
			sb.UseOpenCodeProvider(url)
			return []string{"XDG_CONFIG_HOME=" + filepath.Join(sb.Home, ".config")}
		},
		args: func(sb *Sandbox, prompt, _ string, resume, _ bool) []string {
			a := []string{"run", "--format", "json", "--dir", sb.Repo}
			if resume {
				a = append(a, "--continue")
			}
			return append(a, prompt)
		},
	},
	{
		harness: "pi", each: forEachPi, modelCall: chatCompletions, failures: openAIFailures, resumes: true,
		provider: func(calls *atomic.Int32, cmd string) http.Handler {
			return openAIBash(calls, cmd, map[string]any{"command": cmd})
		},
		configure: func(sb *Sandbox, url string) []string {
			sb.UsePiProvider(url)
			return []string{"PI_CODING_AGENT_DIR=" + sb.piAgentDir()}
		},
		args: func(_ *Sandbox, prompt, session string, _, _ bool) []string {
			return []string{"-p", "--model", "fake/m", "--offline", "--session-id", session, prompt}
		},
	},
	{
		harness: "omp", each: forEachOmp, modelCall: chatCompletions, failures: openAIFailures, resumes: true,
		provider: func(calls *atomic.Int32, cmd string) http.Handler {
			return openAIBash(calls, cmd, map[string]any{"command": cmd})
		},
		configure: func(sb *Sandbox, url string) []string { sb.UseOmpProvider(url); return nil },
		args: func(_ *Sandbox, prompt, _ string, resume, _ bool) []string {
			a := []string{"-p", "--model", "fake/m"}
			if resume {
				a = append(a, "--continue")
			}
			return append(a, prompt)
		},
	},
	{
		harness: "hermes", each: forEachHermes, modelCall: chatCompletions, failures: openAIFailures, resumes: true,
		provider: func(calls *atomic.Int32, cmd string) http.Handler {
			if cmd == "" {
				return hermesProvider(calls, "", nil)
			}
			return hermesProvider(calls, "terminal", map[string]any{"command": cmd})
		},
		configure: func(sb *Sandbox, url string) []string { sb.UseHermes(url); return nil },
		args: func(_ *Sandbox, prompt, _ string, resume, tools bool) []string {
			a := []string{"chat", "-Q"}
			if resume {
				a = append(a, "--continue")
			}
			if tools {
				a = append(a, "--yolo")
			}
			return append(a, "-q", prompt)
		},
	},
	{
		harness: "dsh", each: forEachDsh, modelCall: anthropicMessages, failures: anthropicFailures,
		provider: func(calls *atomic.Int32, cmd string) http.Handler {
			return claudeProbeProvider(calls, shellStep("bash", cmd), nil)
		},
		configure: func(sb *Sandbox, url string) []string { sb.UseDsh(url); return nil },
		args: func(_ *Sandbox, prompt, _ string, _, _ bool) []string {
			return []string{"headless", prompt}
		},
	},
	{
		harness: "copilot", each: forEachCopilotBuild, modelCall: anthropicMessages, failures: anthropicFailures, resumes: true,
		provider: func(calls *atomic.Int32, cmd string) http.Handler {
			return claudeProbeProvider(calls, shellStep("bash", cmd), nil)
		},
		configure: func(sb *Sandbox, url string) []string { return copilotFixtureEnv(sb, url) },
		args: func(_ *Sandbox, prompt, _ string, resume, tools bool) []string {
			var a []string
			if resume {
				a = append(a, "--continue")
			}
			if tools {
				a = append(a, "--allow-all-tools")
			}
			return append(a, "-p", prompt)
		},
	},
}

// harnessProbe runs probe for each build of each harness in drivers that keep selects, unless
// TERMA_E2E_PROBES leaves out every one of names.
func harnessProbe(t *testing.T, names []string, keep func(harnessDriver) bool, probe func(t *testing.T, d harnessDriver, b Binary)) {
	t.Helper()
	skipUnlessProbe(t, names...)
	for _, d := range harnessDrivers {
		if !keep(d) {
			continue
		}
		t.Run(d.harness, func(t *testing.T) {
			d.each(t, func(t *testing.T, b Binary) {
				track(t)
				profileOf(t, b) // the build's test, not a probe's subtest, says whether it failed
				probe(t, d, b)
			})
		})
	}
}

// harnessFixture is a sandbox of t's whose harness d has provider as its model provider, and the
// environment b runs with there.
func harnessFixture(t *testing.T, d harnessDriver, b Binary, provider http.Handler) (*Sandbox, []string) {
	t.Helper()
	sb := New(t, Isolated)
	PlaceSandbox(t, b, sb)
	placeUnresolved(t, b, sb)
	srv := httptest.NewServer(provider)
	t.Cleanup(srv.Close)
	Placeholder(t, b, srv.URL, "<provider>")
	extra := d.configure(sb, srv.URL) // first: it may add to the sandbox's own environment
	return sb, append(sb.termaEnv(), extra...)
}

// harnessRun runs b with args in sb's repository and env, bounded by probeTimeout, and returns
// what it printed.
func harnessRun(sb *Sandbox, b Binary, env, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.Path, args...)
	cmd.Dir = sb.Repo
	cmd.Env = env
	cmd.Stdin = nil
	cmd.WaitDelay = 5 * time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if ctx.Err() != nil {
		err = fmt.Errorf("did not finish in %v", probeTimeout)
	}
	if err != nil {
		return out.String(), fmt.Errorf("%s: %w\n%s", b.Harness, err, tail(out.String(), 2000))
	}
	return out.String(), nil
}

// TestHarnessProbeAcrossTurns: three turns of one session of each harness that can resume one
// headless, so refgen sees how its cache keys and the prompt prefix carry across turns.
// ~5–15 s a build.
func TestHarnessProbeAcrossTurns(t *testing.T) {
	harnessProbe(t, []string{"across-turns"}, func(d harnessDriver) bool { return d.resumes }, func(t *testing.T, d harnessDriver, b Binary) {
		var calls atomic.Int32
		sb, env := harnessFixture(t, d, b, CaptureProbe(t, b, "across-turns", d.provider(&calls, "")))
		session := uuid.NewString()
		Placeholder(t, b, session, "<session>")
		var commands [][]string
		for turn := 1; turn <= 3; turn++ {
			prompt, name := profilePrompt, "<prompt>"
			if turn > 1 {
				prompt, name = fmt.Sprintf("Say hello again (%d). TERMA_PROFILE", turn), fmt.Sprintf("<prompt-%d>", turn)
			}
			Placeholder(t, b, prompt, name)
			args := d.args(sb, prompt, session, turn > 1, false)
			commands = append(commands, append([]string{b.Harness}, args[:len(args)-1]...))
			if _, err := harnessRun(sb, b, env, args); err != nil {
				t.Fatalf("turn %d: %v", turn, err)
			}
		}
		RecordCommand(t, b, commands[0][1:])
		RecordProbe(t, b, "across-turns", map[string]any{"turns": 3, "commands": commands})
		requireCalls(t, b, &calls)
	})
}

// TestHarnessProbeRetries: the provider fails each harness's first three model calls with each of
// its API's retryable statuses (429 and 500, or 529 where it speaks Anthropic's), retry-after: 2,
// then answers; RecordProbe("retries-<status>") says how the run ended. A build's statuses run at
// once: ~10 s a build, ~45 s for Gemini, which keeps its own backoff (5, 8, 25 s); a harness that
// waits longer is stopped at probeTimeout.
func TestHarnessProbeRetries(t *testing.T) {
	names := []string{"retries-429", "retries-500", "retries-529"}
	harnessProbe(t, names, func(harnessDriver) bool { return true }, func(t *testing.T, d harnessDriver, b Binary) {
		for _, f := range d.failures {
			probe := fmt.Sprintf("retries-%d", f.status)
			if !probeOn(probe) {
				continue
			}
			t.Run(probe, func(t *testing.T) {
				t.Parallel() // a build's statuses at once: Gemini alone waits ~40 s through one
				sb, env := harnessFixture(t, d, b, CaptureProbe(t, b, probe,
					failingFirst(retryFailures, f.status, retryAfter, f.body, d.modelCall, d.provider(new(atomic.Int32), ""))))
				Placeholder(t, b, profilePrompt, "<prompt>")
				args := d.args(sb, profilePrompt, uuid.NewString(), false, false)
				RecordCommand(t, b, args[:len(args)-1])
				found := retryProbe{Status: f.status, FailedFirst: retryFailures, RetryAfterSeconds: retryAfter}
				if _, err := harnessRun(sb, b, env, args); err != nil {
					found.Error = firstLine(err.Error())
				} else {
					found.Succeeded = true
				}
				RecordProbe(t, b, probe, found)
			})
		}
	})
}

// TestHarnessProbeToolOutput: each harness's shell tool runs a command printing about 450 KB of
// numbered lines (largeOutput), so the request after it shows what of that the build sends back.
// The shell tool is let run where headless runs would not otherwise (the command says how).
// ~5–10 s a build.
func TestHarnessProbeToolOutput(t *testing.T) {
	harnessProbe(t, []string{"tool-output"}, func(harnessDriver) bool { return true }, func(t *testing.T, d harnessDriver, b Binary) {
		out := largeOutput()
		var calls atomic.Int32
		sb, env := harnessFixture(t, d, b, CaptureProbe(t, b, "tool-output", d.provider(&calls, out.Command)))
		const prompt = "Run it. TERMA_PROFILE"
		Placeholder(t, b, prompt, "<prompt>")
		args := d.args(sb, prompt, uuid.NewString(), false, true)
		RecordCommand(t, b, args[:len(args)-1])
		if _, err := harnessRun(sb, b, env, args); err != nil {
			t.Fatal(err)
		}
		if calls.Load() < 2 {
			t.Errorf("%s never sent the tool's output back (%d model requests)", b.Harness, calls.Load())
		}
		RecordProbe(t, b, "tool-output", out)
	})
}
