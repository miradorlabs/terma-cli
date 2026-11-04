package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Behaviour probes for Claude Code and Codex (probes.go says what a probe is). Each runs the
// build as TestClaudeProfile and TestCodexProfile do, with its own defaults: what a probe sets
// beyond them (a tool allowed, a session resumed) is in its recorded command. Costs per build,
// as measured on a laptop, are in each probe's comment.

// --- Claude Code ---------------------------------------------------------------------

// claudeProbe runs probe for each Claude Code build in a sandbox of its own, with the synthetic
// key, unless TERMA_E2E_PROBES leaves out every one of names.
func claudeProbe(t *testing.T, names []string, probe func(t *testing.T, b Binary, sb *Sandbox)) {
	t.Helper()
	claudeProbeBuilds(t, names, func(t *testing.T, b Binary) { probe(t, b, newClaudeProbeSandbox(t, b)) })
}

// claudeProbeBuilds is claudeProbe for a probe that makes its sandboxes itself, one a subtest.
func claudeProbeBuilds(t *testing.T, names []string, probe func(t *testing.T, b Binary)) {
	t.Helper()
	skipUnlessProbe(t, names...)
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		profileOf(t, b) // the build's test, not a probe's subtest, says whether it failed
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		probe(t, b)
	})
}

func newClaudeProbeSandbox(t *testing.T, b Binary) *Sandbox {
	t.Helper()
	sb := New(t, Isolated, WithClaude(b))
	PlaceSandbox(t, b, sb)
	Placeholder(t, b, claudeProjectDir(sb.Repo), "<project>")
	return sb
}

// claudeProjectDir is the name Claude Code gives the directory under projects/ of a session in
// dir (its transcripts, the tool results it saves): dir with every other character than a
// letter or digit a dash.
func claudeProjectDir(dir string) string {
	return strings.Map(func(r rune) rune {
		if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, dir)
}

// serveClaude serves h as sb's Claude Code provider for the rest of t.
func serveClaude(t *testing.T, b Binary, sb *Sandbox, h http.Handler) {
	t.Helper()
	provider := httptest.NewServer(h)
	t.Cleanup(provider.Close)
	sb.ClaudeBaseURL = provider.URL
	Placeholder(t, b, provider.URL, "<provider>")
}

// claudeRun runs `claude -p prompt` with args in sb, bounded by probeTimeout, and returns the
// JSON it printed (nil where it printed none). A build that printed its result and was slow only
// to exit (its exporter flushing to a relay that is not running) has done what the probe needs.
func claudeRun(sb *Sandbox, prompt string, args ...string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, sb.claudeLauncher(), append([]string{"-p", prompt}, args...)...)
	cmd.Dir = sb.workDir()
	cmd.Env = sb.claudeEnv(RouteAPIKey)
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var result map[string]any
	_ = json.Unmarshal(stdout.Bytes(), &result)
	if ctx.Err() != nil {
		if result["type"] == "result" && result["is_error"] == false {
			sb.T.Logf("claude -p printed its result but did not exit in %v", probeTimeout)
			return result, nil
		}
		err = fmt.Errorf("did not finish in %v", probeTimeout)
	}
	if err != nil {
		return result, fmt.Errorf("claude -p: %w\n%s\n%s", err, tail(stdout.String(), 2000), tail(stderr.String(), 2000))
	}
	return result, nil
}

// TestClaudeProbeAcrossTurns: three turns of one session, the second and third resumed with
// --resume, so refgen sees where the cache breakpoints move and which prefix stays the same.
// ~10 s a build.
func TestClaudeProbeAcrossTurns(t *testing.T) {
	claudeProbe(t, []string{"across-turns"}, func(t *testing.T, b Binary, sb *Sandbox) {
		serveClaude(t, b, sb, CaptureProbe(t, b, "across-turns", claudeProbeProvider(new(atomic.Int32), nil, nil)))
		sid := uuid.NewString()
		Placeholder(t, b, sid, "<session>")
		var commands [][]string
		for turn := 1; turn <= 3; turn++ {
			prompt, name := profilePrompt, "<prompt>"
			args := []string{"--output-format", "json", "--max-turns", "1", "--session-id", sid}
			if turn > 1 {
				prompt, name = fmt.Sprintf("Say hello again (%d). TERMA_PROFILE", turn), fmt.Sprintf("<prompt-%d>", turn)
				args[len(args)-2] = "--resume"
			}
			Placeholder(t, b, prompt, name)
			commands = append(commands, append([]string{"claude"}, args...))
			if _, err := claudeRun(sb, prompt, args...); err != nil {
				t.Fatalf("turn %d: %v", turn, err)
			}
		}
		RecordCommand(t, b, commands[0][1:])
		RecordProbe(t, b, "across-turns", map[string]any{"turns": 3, "commands": commands})
	})
}

// TestClaudeProbeCompaction: a session the fixture says is near its context window, so the next
// turn compacts first. The turn that sets it up is under "compaction-setup"; the resumed turn,
// whose first request is the compaction itself (its prompt and model), under "compaction".
// Then /compact, Claude Code's own command, under "compaction-command". ~15 s a build.
func TestClaudeProbeCompaction(t *testing.T) {
	claudeProbe(t, []string{"compaction"}, func(t *testing.T, b Binary, sb *Sandbox) {
		var full atomic.Bool
		full.Store(true)
		usage := func([]byte) map[string]any {
			if full.Load() {
				return claudeUsage(compactionTokens, 4, 0, 0, 0)
			}
			return nil
		}
		capture := captureSwitch(t, b, "compaction-setup", claudeProbeProvider(new(atomic.Int32), nil, usage))
		serveClaude(t, b, sb, capture)
		sid := uuid.NewString()
		Placeholder(t, b, sid, "<session>")
		Placeholder(t, b, profilePrompt, "<prompt>")
		args := []string{"--output-format", "json", "--max-turns", "1", "--session-id", sid}
		RecordCommand(t, b, args)
		if _, err := claudeRun(sb, profilePrompt, args...); err != nil {
			t.Fatal(err)
		}
		capture.Set("compaction")
		full.Store(false)
		const next = "Say hello once more. TERMA_PROFILE"
		Placeholder(t, b, next, "<prompt-2>")
		// An older build does not compact a context past its window on its own: it refuses the
		// turn ("Prompt is too long"), which is what it does, not a failed probe.
		auto := map[string]any{"trigger": "auto", "reported_input_tokens": compactionTokens}
		if result, err := claudeRun(sb, next, "--output-format", "json", "--max-turns", "1", "--resume", sid); err != nil {
			if !refusedTooLong(result) {
				t.Fatal(err)
			}
			auto["outcome"] = "refused"
			auto["reason"] = result["result"]
		} else {
			auto["outcome"] = "compacted"
		}
		capture.Set("compaction-command")
		manual := map[string]any{"trigger": "/compact"}
		if result, err := claudeRun(sb, "/compact", "--output-format", "json", "--max-turns", "1", "--resume", sid); err != nil {
			if !refusedTooLong(result) {
				t.Fatal(err)
			}
			manual["outcome"] = "refused"
			manual["reason"] = result["result"]
		} else {
			manual["outcome"] = "compacted"
		}
		RecordProbe(t, b, "compaction", auto)
		RecordProbe(t, b, "compaction-command", manual)
	})
}

// refusedTooLong reports a turn Claude Code refused because its context was past the window.
func refusedTooLong(result map[string]any) bool {
	reason, _ := result["terminal_reason"].(string)
	text, _ := result["result"].(string)
	return reason == "blocking_limit" || strings.Contains(text, "Prompt is too long")
}

// compactionTokens is the input a compaction probe's fixture reports: more than any build's
// context window less its compaction buffer.
const compactionTokens = 995_000

// TestClaudeProbeRetries: the provider fails the first three calls with 429, then 529, each
// with retry-after: 2, and then answers; the arrival times are the build's backoff, and a model
// changed on a retry is its fallback. ~10 s a status a build.
func TestClaudeProbeRetries(t *testing.T) {
	claudeProbeBuilds(t, []string{"retries-429", "retries-529"}, func(t *testing.T, b Binary) {
		for _, f := range []struct {
			status int
			body   string
		}{
			{429, `{"type":"error","error":{"type":"rate_limit_error","message":"Rate limited"}}`},
			{529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`},
		} {
			probe := fmt.Sprintf("retries-%d", f.status)
			if !probeOn(probe) {
				continue
			}
			t.Run(probe, func(t *testing.T) {
				sb := newClaudeProbeSandbox(t, b)
				messages := func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/v1/messages") }
				serveClaude(t, b, sb, CaptureProbe(t, b, probe, failingFirst(retryFailures, f.status, retryAfter, f.body, messages,
					claudeProbeProvider(new(atomic.Int32), nil, nil))))
				Placeholder(t, b, profilePrompt, "<prompt>")
				args := []string{"--output-format", "json", "--max-turns", "1", "--session-id", uuid.NewString()}
				RecordCommand(t, b, args)
				found := retryProbe{Status: f.status, FailedFirst: retryFailures, RetryAfterSeconds: retryAfter}
				if result, err := claudeRun(sb, profilePrompt, args...); err != nil {
					found.Error = claudeError(result, err)
				} else {
					found.Succeeded = true
				}
				RecordProbe(t, b, probe, found)
			})
		}
	})
}

// A retries probe's provider fails retryFailures calls, each saying to retry after retryAfter
// seconds.
const (
	retryFailures = 3
	retryAfter    = 2
)

// claudeError is what a failed `claude -p` said of its failure: its JSON result's text, or the
// first line of err.
func claudeError(result map[string]any, err error) string {
	if text, _ := result["result"].(string); text != "" {
		return text
	}
	if errs, _ := result["errors"].([]any); len(errs) > 0 {
		return fmt.Sprint(errs...)
	}
	return firstLine(err.Error())
}

// firstLine is s up to its first newline.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// TestClaudeProbeSubagent: the model asks for a general-purpose subagent through the Agent tool,
// so the subagent's own request (its system prompt, model and tools) is captured beside the
// main thread's. The subagent's task is the placeholder <subagent-prompt>. ~8 s a build.
func TestClaudeProbeSubagent(t *testing.T) {
	claudeProbe(t, []string{"subagent"}, func(t *testing.T, b Binary, sb *Sandbox) {
		const task = "Reply with one word. TERMA_SUBAGENT_TASK"
		steps := []claudeStep{{{"name": "Agent", "input": map[string]any{"description": "check", "prompt": task, "subagent_type": "general-purpose"}}}}
		var calls atomic.Int32
		serveClaude(t, b, sb, CaptureProbe(t, b, "subagent", claudeProbeProvider(&calls, steps, nil)))
		const prompt = "Use a subagent. TERMA_PROFILE"
		Placeholder(t, b, prompt, "<prompt>")
		Placeholder(t, b, task, "<subagent-prompt>")
		args := []string{"--output-format", "json", "--max-turns", "3", "--session-id", uuid.NewString()}
		RecordCommand(t, b, args)
		if _, err := claudeRun(sb, prompt, args...); err != nil {
			t.Fatal(err)
		}
		if calls.Load() < 3 {
			t.Errorf("%d message requests: the main thread's two and the subagent's at least were expected", calls.Load())
		}
	})
}

// TestClaudeProbeToolOutput: the model runs a Bash command printing about 450 KB of numbered
// lines (largeOutput), so the request after it shows what of that the build sends the model:
// its limit, where it cuts, and the marker. Bash is allowed, as a user's approval would.
// ~6 s a build.
func TestClaudeProbeToolOutput(t *testing.T) {
	claudeProbe(t, []string{"tool-output"}, func(t *testing.T, b Binary, sb *Sandbox) {
		out := largeOutput()
		var calls atomic.Int32
		serveClaude(t, b, sb, CaptureProbe(t, b, "tool-output", claudeProbeProvider(&calls, []claudeStep{{bash(out.Command)}}, nil)))
		const prompt = "Run it. TERMA_PROFILE"
		Placeholder(t, b, prompt, "<prompt>")
		args := []string{"--output-format", "json", "--max-turns", "3", "--allowedTools", "Bash", "--session-id", uuid.NewString()}
		RecordCommand(t, b, args)
		if _, err := claudeRun(sb, prompt, args...); err != nil {
			t.Fatal(err)
		}
		if calls.Load() < 2 {
			t.Errorf("the build never sent the tool's output back (%d message requests)", calls.Load())
		}
		RecordProbe(t, b, "tool-output", out)
	})
}

// claudeAliases are the --model aliases a probe resolves, each its own model-alias-<alias>.
var claudeAliases = []string{"sonnet", "opus", "haiku", "opusplan", "sonnet[1m]", "opus[1m]"}

// TestClaudeProbeModelAliases: one turn with each --model alias, so the model each request
// names is the alias resolved. An alias the build refuses is recorded as such. ~3 s an alias a
// build.
func TestClaudeProbeModelAliases(t *testing.T) {
	var names []string
	for _, a := range claudeAliases {
		names = append(names, "model-alias-"+a)
	}
	claudeProbe(t, names, func(t *testing.T, b Binary, sb *Sandbox) {
		capture := captureSwitch(t, b, names[0], claudeProbeProvider(new(atomic.Int32), nil, nil))
		serveClaude(t, b, sb, capture)
		Placeholder(t, b, profilePrompt, "<prompt>")
		RecordCommand(t, b, []string{"--output-format", "json", "--max-turns", "1", "--model", "<alias>"})
		for i, alias := range claudeAliases {
			if !probeOn(names[i]) {
				continue
			}
			capture.Set(names[i])
			if result, err := claudeRun(sb, profilePrompt, "--output-format", "json", "--max-turns", "1", "--model", alias); err != nil {
				RecordProbe(t, b, names[i], map[string]any{"error": claudeError(result, err)})
			}
		}
	})
}

// priceTokens is how many tokens of one kind a prices probe's reply reports: well under the
// 200,000 above which a model may be priced as long context.
const priceTokens = 100_000

// priceKinds are the usages a prices probe's replies report, one a reply, by the key RecordProbe
// gives each price, with how many tokens of it: the two cache writes differ in size, which is
// how an api_request record, which sums them, says which it was.
var priceKinds = []struct {
	key    string
	tokens int
	usage  map[string]any
}{
	{"input", priceTokens, claudeUsage(priceTokens, 0, 0, 0, 0)},
	{"output", priceTokens, claudeUsage(0, priceTokens, 0, 0, 0)},
	{"cache_read", priceTokens, claudeUsage(0, 0, priceTokens, 0, 0)},
	{"cache_write_5m", priceTokens, claudeUsage(0, 0, 0, priceTokens, 0)},
	{"cache_write_1h", priceTokens / 2, claudeUsage(0, 0, 0, 0, priceTokens/2)},
}

// priceKind is the kind of usage a Claude Code api_request record reports and how many tokens of
// it: its one count that is not zero.
func priceKind(attrs map[string]string) (string, int, bool) {
	n := func(key string) int { v, _ := strconv.Atoi(attrs[key]); return v }
	in, out, read, write := n("input_tokens"), n("output_tokens"), n("cache_read_tokens"), n("cache_creation_tokens")
	switch {
	case in > 0 && out+read+write == 0:
		return "input", in, true
	case out > 0 && in+read+write == 0:
		return "output", out, true
	case read > 0 && in+out+write == 0:
		return "cache_read", read, true
	case write > 0 && in+out+read == 0:
		for _, k := range priceKinds[3:] {
			if write == k.tokens {
				return k.key, write, true
			}
		}
	}
	return "", 0, false
}

// TestClaudeProbePrices: what Claude Code charges itself for each model it resolves (its
// default, and each of sonnet, opus and haiku). Each model's run is a tool loop of five model
// calls whose replies report one kind of usage each (priceKinds), and Claude Code's own
// api_request telemetry says what each cost (cost_usd, by its model). RecordProbe("prices",
// {model: {input, output, cache_read, cache_write_5m, cache_write_1h, unit}}), in USD per million
// tokens. No request is kept. Four runs, ~30 s a build.
func TestClaudeProbePrices(t *testing.T) {
	claudeProbe(t, []string{"prices"}, func(t *testing.T, b Binary, sb *Sandbox) {
		sb.directClaude() // the build's telemetry to the receiver, where api_request says each call's cost
		steps := make([]claudeStep, len(priceKinds)-1)
		for i := range steps {
			steps[i] = claudeStep{{"name": "Read", "input": map[string]any{"file_path": filepath.Join(sb.Repo, "README.md")}}}
		}
		RecordCommand(t, b, []string{"--output-format", "json", "--max-turns", "8", "--model", "<alias>"})
		prices := map[string]map[string]any{}
		priced := func(model string) bool { return len(prices[model]) == len(priceKinds)+1 }
		for _, alias := range []string{"", "sonnet", "opus", "haiku"} {
			// A run whose telemetry does not all arrive (a build can exit before its last export)
			// is run again, once, unless its model is priced already.
			for attempt := 1; attempt <= 2; attempt++ {
				model, complete := priceRun(t, b, sb, steps, alias, prices)
				if complete || priced(model) {
					break
				}
				t.Logf("%q (%s): not every call's cost arrived (attempt %d)", alias, model, attempt)
			}
		}
		for model, p := range prices {
			if !priced(model) {
				t.Errorf("%s: priced only %v", model, p)
			}
		}
		RecordProbe(t, b, "prices", prices)
	})
}

// priceRun is one prices run of the model alias names (the default where it is empty): it adds
// what the api_request records say to prices, and returns the model the run used and whether
// every call's cost arrived.
func priceRun(t *testing.T, b Binary, sb *Sandbox, steps []claudeStep, alias string, prices map[string]map[string]any) (string, bool) {
	t.Helper()
	var reply atomic.Int32
	serveClaude(t, b, sb, claudeProbeProvider(new(atomic.Int32), steps, func([]byte) map[string]any {
		if i := int(reply.Add(1)) - 1; i < len(priceKinds) {
			return priceKinds[i].usage
		}
		return nil
	}))
	sid := uuid.NewString()
	args := []string{"--output-format", "json", "--max-turns", "8", "--session-id", sid}
	if alias != "" {
		args = append(args, "--model", alias)
	}
	result, err := claudeRun(sb, profilePrompt, args...)
	if err != nil {
		t.Fatalf("%q: %v", alias, err)
	}
	var model string
	usage, _ := result["modelUsage"].(map[string]any)
	for name := range usage {
		model = name
	}
	var records []LogRecord
	for deadline := time.Now().Add(20 * time.Second); len(records) < len(priceKinds) && time.Now().Before(deadline); {
		records = sb.APIRequests(sid, time.Second)
	}
	for _, r := range records {
		kind, tokens, ok := priceKind(r.Attrs)
		cost, err := strconv.ParseFloat(r.Attrs["cost_usd"], 64)
		if !ok || err != nil || r.Attrs["model"] == "" {
			t.Errorf("%q: an api_request this probe cannot price: %v", alias, r.Attrs)
			continue
		}
		if prices[r.Attrs["model"]] == nil {
			prices[r.Attrs["model"]] = map[string]any{"unit": "USD per million tokens"}
		}
		prices[r.Attrs["model"]][kind] = perMillion(cost, tokens)
	}
	return model, len(records) >= len(priceKinds)
}

// perMillion is cost for tokens as a price per million, to a millionth of a dollar.
func perMillion(cost float64, tokens int) float64 {
	return math.Round(cost/float64(tokens)*1e6*1e6) / 1e6
}

// TestClaudeProbeInteractive: Claude Code's first turn in an interactive session, in a real
// pty, as a person starts it (`claude`, no flag but a session id): its system prompt, tools and
// side requests differ from `claude -p`'s. The synthetic key is approved, as a developer's own
// is. ~15 s a build.
func TestClaudeProbeInteractive(t *testing.T) {
	claudeProbe(t, []string{"interactive"}, func(t *testing.T, b Binary, sb *Sandbox) {
		approveClaudeKey(t, sb, "synthetic-telemetry-key")
		var calls atomic.Int32
		serveClaude(t, b, sb, CaptureProbe(t, b, "interactive", claudeProbeProvider(&calls, nil, nil)))
		Placeholder(t, b, profilePrompt, "<prompt>")
		sid := uuid.NewString()
		Placeholder(t, b, sid, "<session>")
		args := []string{"--session-id", sid}
		RecordCommand(t, b, args)
		term, err := Start(sb.Repo, sb.claudeEnv(RouteAPIKey), 40, 120, sb.claudeLauncher(), args...)
		if err != nil {
			t.Fatalf("start claude: %v", err)
		}
		defer func() { _ = term.Close("", 2*time.Second) }()
		if err := pastClaudeDialogs(term, probeTimeout); err != nil {
			t.Fatalf("%v\n%s", err, tail(term.Text(), 3000))
		}
		time.Sleep(1500 * time.Millisecond)
		term.Consume()
		if err := term.Type(profilePrompt); err != nil {
			t.Fatal(err)
		}
		if _, err := term.Expect(regexp.MustCompile(`TERMA_TELEMETRY_REPLY`), probeTimeout); err != nil {
			t.Fatalf("%v\n%s", err, tail(term.Text(), 3000))
		}
		time.Sleep(3 * time.Second) // what the session sends after the reply
		_ = term.Send("\x03")
		time.Sleep(400 * time.Millisecond)
		_ = term.Send("\x03")
		if err := term.Close("", 10*time.Second); err != nil {
			t.Logf("claude exit: %v", err)
		}
		if calls.Load() == 0 {
			t.Fatal("the interactive session sent its provider no message request")
		}
	})
}

// approveClaudeKey records key as one the developer approved, so an interactive session uses it
// without asking.
func approveClaudeKey(t *testing.T, sb *Sandbox, key string) {
	t.Helper()
	path := filepath.Join(sb.ClaudeConfig, ".claude.json")
	state := map[string]any{}
	raw, _ := os.ReadFile(path)
	_ = json.Unmarshal(raw, &state)
	state["customApiKeyResponses"] = map[string]any{"approved": []string{key[len(key)-20:]}, "rejected": []string{}}
	raw, _ = json.MarshalIndent(state, "", "  ")
	sb.writeAbs(path, string(raw))
}

// pastClaudeDialogs answers the dialogs a fresh interactive session may show until its prompt,
// as ClaudeInteractiveTurns does.
func pastClaudeDialogs(term *Terminal, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		i, err := term.ExpectAny(time.Until(deadline), loginRE, trustRE, themeRE, bypassRE, promptRE)
		switch {
		case err != nil:
			return fmt.Errorf("claude never showed a prompt: %w", err)
		case i == 0:
			return errors.New("claude asked to log in")
		case i == 1:
			time.Sleep(500 * time.Millisecond)
			if yesSecondRE.MatchString(term.TextSince()) {
				_ = term.Send("\x1b[B")
				time.Sleep(300 * time.Millisecond)
			}
			term.Consume()
			_ = term.Send("\r")
		case i == 4:
			return nil
		default:
			term.Consume()
			_ = term.Send("\r")
		}
	}
}

// --- Codex ---------------------------------------------------------------------------

// codexProbe runs probe for each Codex build in a sandbox of its own, logged in with the
// synthetic key, unless TERMA_E2E_PROBES leaves out every one of names.
func codexProbe(t *testing.T, names []string, probe func(t *testing.T, b Binary, sb *Sandbox)) {
	t.Helper()
	codexProbeBuilds(t, names, func(t *testing.T, b Binary) { probe(t, b, newCodexProbeSandbox(t, b)) })
}

// codexProbeBuilds is codexProbe for a probe that makes its sandboxes itself, one a subtest.
func codexProbeBuilds(t *testing.T, names []string, probe func(t *testing.T, b Binary)) {
	t.Helper()
	skipUnlessProbe(t, names...)
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		profileOf(t, b)
		t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
		probe(t, b)
	})
}

func newCodexProbeSandbox(t *testing.T, b Binary) *Sandbox {
	t.Helper()
	sb := New(t, Isolated, WithCodex(b))
	sb.prepareCodex(RouteAPIKey)
	PlaceSandbox(t, b, sb)
	return sb
}

// serveCodex serves h as the fixture provider for the rest of t, and returns the -c options
// that make it Codex's provider, as TestCodexProfile sets them.
func serveCodex(t *testing.T, b Binary, h http.Handler) []string {
	t.Helper()
	provider := httptest.NewServer(h)
	t.Cleanup(provider.Close)
	Placeholder(t, b, provider.URL, "<provider>")
	return []string{"-c", `cli_auth_credentials_store="file"`, "-c", "features.plugins=false", "-c", "features.remote_plugin=false",
		"-c", `model_provider="telemetry_fixture"`, "-c", `model_providers.telemetry_fixture.name="Telemetry fixture"`,
		"-c", "model_providers.telemetry_fixture.base_url=" + tomlQuote(provider.URL),
		"-c", `model_providers.telemetry_fixture.wire_api="responses"`,
		"-c", `model_providers.telemetry_fixture.requires_openai_auth=false`}
}

// codexExecArgs is TestCodexProfile's `codex exec` with config, the prompt left out.
func codexExecArgs(sb *Sandbox, config []string) []string {
	return append(append(append([]string{"exec", "--json"}, codexOptionalFlags(sb)...), "-C", sb.workDir()), config...)
}

// codexResumeArgs resumes thread with config, the prompt left out.
func codexResumeArgs(sb *Sandbox, config []string, thread string) []string {
	return append(append(append([]string{"exec", "resume", "--json"}, codexOptionalFlags(sb)...), config...), thread)
}

// codexRun runs Codex with args and prompt in sb, bounded by probeTimeout, and returns the
// thread it started (from --json's thread.started, "" without --json) and what it printed.
func codexRun(sb *Sandbox, args []string, prompt string) (thread, stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, sb.Codex.Path, append(args, prompt)...)
	cmd.Dir = sb.workDir()
	cmd.Env = sb.codexEnv(RouteAPIKey)
	cmd.WaitDelay = 5 * time.Second
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	completed := false
	for line := range strings.Lines(out.String()) {
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		switch ev["type"] {
		case "thread.started":
			thread, _ = ev["thread_id"].(string)
		case "turn.completed":
			completed = true
		}
	}
	if ctx.Err() != nil {
		// As claudeRun: a turn that completed and a build slow only to exit.
		if completed {
			sb.T.Logf("codex printed its completed turn but did not exit in %v", probeTimeout)
			return thread, out.String(), errOut.String(), nil
		}
		err = fmt.Errorf("did not finish in %v", probeTimeout)
	}
	if err != nil {
		err = fmt.Errorf("codex %s: %w\n%s\n%s", args[0], err, tail(out.String(), 2000), tail(errOut.String(), 2000))
	}
	return thread, out.String(), errOut.String(), err
}

// codexError is what a failed `codex exec --json` said of its failure: the message of its last
// error or turn.failed event, or the first line of err.
func codexError(stdout string, err error) string {
	var said string
	for line := range strings.Lines(stdout) {
		var ev struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		switch {
		case ev.Type == "error" && ev.Message != "":
			said = ev.Message
		case ev.Type == "turn.failed" && ev.Error.Message != "":
			said = ev.Error.Message
		}
	}
	if said != "" {
		return said
	}
	return firstLine(err.Error())
}

// TestCodexProbeAcrossTurns: three turns of one thread, the second and third with `codex exec
// resume`, so refgen sees how prompt_cache_key and the input prefix carry across turns.
// ~15 s a build.
func TestCodexProbeAcrossTurns(t *testing.T) {
	codexProbe(t, []string{"across-turns"}, func(t *testing.T, b Binary, sb *Sandbox) {
		config := serveCodex(t, b, CaptureProbe(t, b, "across-turns", codexProbeProvider(new(atomic.Int32), nil, nil)))
		args := codexExecArgs(sb, config)
		RecordCommand(t, b, args)
		Placeholder(t, b, profilePrompt, "<prompt>")
		thread, _, _, err := codexRun(sb, args, profilePrompt)
		if err != nil || thread == "" {
			t.Fatalf("turn 1 (thread %q): %v", thread, err)
		}
		Placeholder(t, b, thread, "<thread>")
		commands := [][]string{append([]string{"codex"}, args...)}
		for turn := 2; turn <= 3; turn++ {
			prompt := fmt.Sprintf("Say hello again (%d). TERMA_PROFILE", turn)
			Placeholder(t, b, prompt, fmt.Sprintf("<prompt-%d>", turn))
			resume := codexResumeArgs(sb, config, thread)
			commands = append(commands, append([]string{"codex"}, resume...))
			if _, _, _, err := codexRun(sb, resume, prompt); err != nil {
				t.Fatalf("turn %d: %v", turn, err)
			}
		}
		RecordProbe(t, b, "across-turns", map[string]any{"turns": 3, "commands": commands})
	})
}

// TestCodexProbeCompaction: a thread the fixture says has used compactionTokens, so the
// resumed turn compacts first. The first turn is under "compaction-setup", the resumed one,
// whose first request is the compaction (its prompt and model), under "compaction". ~12 s a
// build.
func TestCodexProbeCompaction(t *testing.T) {
	codexProbe(t, []string{"compaction"}, func(t *testing.T, b Binary, sb *Sandbox) {
		var full atomic.Bool
		full.Store(true)
		usage := func() map[string]any {
			if full.Load() {
				return codexUsage(compactionTokens, 0, 4)
			}
			return nil
		}
		capture := captureSwitch(t, b, "compaction-setup", codexProbeProvider(new(atomic.Int32), nil, usage))
		config := serveCodex(t, b, capture)
		args := codexExecArgs(sb, config)
		RecordCommand(t, b, args)
		Placeholder(t, b, profilePrompt, "<prompt>")
		thread, _, _, err := codexRun(sb, args, profilePrompt)
		if err != nil || thread == "" {
			t.Fatalf("turn 1 (thread %q): %v", thread, err)
		}
		Placeholder(t, b, thread, "<thread>")
		capture.Set("compaction")
		full.Store(false)
		const next = "Say hello once more. TERMA_PROFILE"
		Placeholder(t, b, next, "<prompt-2>")
		if _, _, _, err := codexRun(sb, codexResumeArgs(sb, config, thread), next); err != nil {
			t.Fatal(err)
		}
		RecordProbe(t, b, "compaction", map[string]any{"trigger": "auto", "reported_input_tokens": compactionTokens})
	})
}

// TestCodexProbeRetries: the provider fails the first three calls with 429, then 500, each
// with retry-after: 2, and then answers; the arrival times are the build's backoff. ~10 s a
// status a build.
func TestCodexProbeRetries(t *testing.T) {
	codexProbeBuilds(t, []string{"retries-429", "retries-500"}, func(t *testing.T, b Binary) {
		for _, f := range []struct {
			status int
			body   string
		}{
			{429, `{"error":{"message":"Rate limit reached","type":"requests","code":"rate_limit_exceeded"}}`},
			{500, `{"error":{"message":"The server had an error","type":"server_error"}}`},
		} {
			probe := fmt.Sprintf("retries-%d", f.status)
			if !probeOn(probe) {
				continue
			}
			t.Run(probe, func(t *testing.T) {
				sb := newCodexProbeSandbox(t, b)
				responses := func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/responses") }
				config := serveCodex(t, b, CaptureProbe(t, b, probe, failingFirst(retryFailures, f.status, retryAfter, f.body, responses,
					codexProbeProvider(new(atomic.Int32), nil, nil))))
				args := codexExecArgs(sb, config)
				RecordCommand(t, b, args)
				Placeholder(t, b, profilePrompt, "<prompt>")
				found := retryProbe{Status: f.status, FailedFirst: retryFailures, RetryAfterSeconds: retryAfter}
				if _, stdout, _, err := codexRun(sb, args, profilePrompt); err != nil {
					found.Error = codexError(stdout, err)
				} else {
					found.Succeeded = true
				}
				RecordProbe(t, b, probe, found)
			})
		}
	})
}

// TestCodexProbeToolOutput: the model runs a shell command printing about 450 KB of numbered
// lines (largeOutput), so the request after it shows what of that Codex sends back: its limit,
// where it cuts, and the marker. ~10 s a build.
func TestCodexProbeToolOutput(t *testing.T) {
	codexProbe(t, []string{"tool-output"}, func(t *testing.T, b Binary, sb *Sandbox) {
		out := largeOutput()
		var calls atomic.Int32
		config := serveCodex(t, b, CaptureProbe(t, b, "tool-output", codexProbeProvider(&calls, []string{out.Command}, nil)))
		args := codexExecArgs(sb, config)
		RecordCommand(t, b, args)
		const prompt = "Run it. TERMA_PROFILE"
		Placeholder(t, b, prompt, "<prompt>")
		if _, _, _, err := codexRun(sb, args, prompt); err != nil {
			t.Fatal(err)
		}
		if calls.Load() < 2 {
			t.Errorf("Codex never sent the tool's output back (%d responses requests)", calls.Load())
		}
		RecordProbe(t, b, "tool-output", out)
	})
}

// TestCodexProbeSubagent: the model spawns a sub-agent (collaboration.spawn_agent) and waits for
// it (wait_agent), so the sub-agent's own request (its instructions, model and tools) is captured
// beside the root thread's. A request is the sub-agent's where its turn metadata names an agent
// other than /root. The sub-agent's task is the placeholder <subagent-prompt>. ~15 s a build.
func TestCodexProbeSubagent(t *testing.T) {
	codexProbe(t, []string{"subagent"}, func(t *testing.T, b Binary, sb *Sandbox) {
		const task = "Reply with one word. TERMA_SUBAGENT_TASK"
		var sub atomic.Int32
		config := serveCodex(t, b, CaptureProbe(t, b, "subagent", codexSubagentProvider(&sub, task)))
		args := codexExecArgs(sb, config)
		RecordCommand(t, b, args)
		const prompt = "Use a sub-agent. TERMA_PROFILE"
		Placeholder(t, b, prompt, "<prompt>")
		Placeholder(t, b, task, "<subagent-prompt>")
		if _, _, _, err := codexRun(sb, args, prompt); err != nil {
			t.Fatal(err)
		}
		if sub.Load() == 0 {
			t.Error("no sub-agent request reached the provider")
		}
	})
}

// codexSubagentProvider answers the root thread's first call with a spawn_agent call for task and
// its second with a wait_agent call, and every other call, a sub-agent's (counted in sub) among
// them, with a reply.
func codexSubagentProvider(sub *atomic.Int32, task string) http.Handler {
	var root atomic.Int32
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Metadata map[string]string `json:"client_metadata"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var turn struct {
			Agent string `json:"agent_name"`
		}
		_ = json.Unmarshal([]byte(req.Metadata["x-codex-turn-metadata"]), &turn)
		if turn.Agent != "" && turn.Agent != "/root" {
			writeCodexResponse(w, sub.Add(1)+100, codexReply())
			return
		}
		switch call := root.Add(1); call {
		case 1:
			a, _ := json.Marshal(map[string]any{"task_name": "check", "message": task})
			writeCodexResponse(w, call, map[string]any{"type": "function_call", "id": "fc_probe_spawn", "call_id": "call_probe_spawn",
				"namespace": "collaboration", "name": "spawn_agent", "arguments": string(a)})
		case 2:
			a, _ := json.Marshal(map[string]any{"timeout_ms": 20000})
			writeCodexResponse(w, call, map[string]any{"type": "function_call", "id": "fc_probe_wait", "call_id": "call_probe_wait",
				"namespace": "collaboration", "name": "wait_agent", "arguments": string(a)})
		default:
			writeCodexResponse(w, call, codexReply())
		}
	})
}

// codexReply is a Responses API assistant message saying the fixture's reply.
func codexReply() map[string]any {
	return map[string]any{"type": "message", "id": "msg_probe", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": "TERMA_TELEMETRY_REPLY", "annotations": []any{}}}}
}

// TestCodexProbeSandbox: the sandbox mode and approval policy a user's first `codex exec` has,
// as Codex states them: the header it prints without --json (RecordProbe("sandbox", {"approval",
// "sandbox", and every other header line by its key})); the request, whose instructions say the
// same to the model, is under "sandbox". ~8 s a build.
func TestCodexProbeSandbox(t *testing.T) {
	codexProbe(t, []string{"sandbox"}, func(t *testing.T, b Binary, sb *Sandbox) {
		config := serveCodex(t, b, CaptureProbe(t, b, "sandbox", codexProbeProvider(new(atomic.Int32), nil, nil)))
		args := append(append(append([]string{"exec"}, codexOptionalFlags(sb)...), "-C", sb.workDir()), config...)
		RecordCommand(t, b, args)
		Placeholder(t, b, profilePrompt, "<prompt>")
		_, stdout, stderr, err := codexRun(sb, args, profilePrompt)
		if err != nil {
			t.Fatal(err)
		}
		header := codexHeader(stderr + "\n" + stdout)
		if header["sandbox"] == "" || header["approval"] == "" {
			t.Fatalf("Codex stated no sandbox or approval policy:\n%s\n%s", tail(stderr, 2000), tail(stdout, 2000))
		}
		RecordProbe(t, b, "sandbox", header)
	})
}

// codexHeader is the settings block `codex exec` prints before a turn ("approval: never",
// "sandbox: read-only", …), by key: the lines between its first two "--------" rules.
func codexHeader(out string) map[string]string {
	header := map[string]string{}
	rules := 0
	for line := range strings.Lines(out) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "--------") {
			if rules++; rules == 2 {
				break
			}
			continue
		}
		if rules != 1 {
			continue
		}
		if key, value, ok := strings.Cut(line, ":"); ok && key != "" && strings.Count(key, " ") <= 2 {
			header[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return header
}
