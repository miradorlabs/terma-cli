package live

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Credentials for Claude Code, from the environment the suite was started with.
type ClaudeCreds struct {
	OAuthToken string // CLAUDE_CODE_OAUTH_TOKEN from `claude setup-token`: a subscription login
	APIKey     string // ANTHROPIC_API_KEY: a Console/API-key route
}

// ClaudeCredentials reads the credential environment.
func ClaudeCredentials() ClaudeCreds {
	return ClaudeCreds{OAuthToken: os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"), APIKey: os.Getenv("ANTHROPIC_API_KEY")}
}

// Route is which credential a run uses.
type Route string

const (
	RouteSubscription Route = "subscription"
	RouteAPIKey       Route = "api_key"
)

// ClaudeRun is one finished scenario.
type ClaudeRun struct {
	SessionID string
	Route     Route
	Term      *Terminal
	Text      string
	Raw       string
}

// Screens a session may show before the prompt. Spacing is loose: a TUI
// positions words with cursor movement as often as with spaces.
var (
	promptRE = regexp.MustCompile(`(?m)^\s*❯`)
	trustRE  = regexp.MustCompile(`(?i)trust\s*this\s*folder|Do\s*you\s*trust`)
	themeRE  = regexp.MustCompile(`(?i)choose\s*the\s*text\s*style|text\s*style\s*that\s*looks\s*best`)
	loginRE  = regexp.MustCompile(`(?i)select\s*login\s*method|Paste\s*code\s*here`)
	bypassRE = regexp.MustCompile(`(?i)bypass\s*permissions\s*mode`)
	// A dialog whose default is to leave: the "yes" answer is the second option.
	yesSecondRE = regexp.MustCompile(`(?i)❯\s*No,?\s*exit`)
)

// claudeEnv is the environment the harness runs with for a route.
func (sb *Sandbox) claudeEnv(route Route) []string {
	env := append(sb.baseEnv(), "TERMA_CONFIG_DIR="+sb.TermaConfig)
	creds := ClaudeCredentials()
	if sb.Mode == Isolated {
		env = append(env, "CLAUDE_CONFIG_DIR="+sb.ClaudeConfig)
		switch route {
		case RouteSubscription:
			env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+creds.OAuthToken)
		case RouteAPIKey:
			if os.Getenv("ANTHROPIC_FEDERATION_RULE_ID") != "" && sb.ClaudeBaseURL == "" {
				federation, err := githubClaudeFederation(sb.Dir)
				if err != nil {
					sb.T.Fatalf("Claude federation: %v", err)
				}
				env = append(env, federation...)
			} else {
				env = append(env, "ANTHROPIC_API_KEY="+creds.APIKey)
			}
		}
	}
	if sb.ClaudeBaseURL != "" {
		env = append(env, "ANTHROPIC_BASE_URL="+sb.ClaudeBaseURL)
	}
	return env
}

// claudeArgs are the flags every scenario shares: a fixed model, no MCP, no
// browser, no skills, a known session id, and in real-login mode Terma's scratch
// settings in place of the developer's own user file.
func (sb *Sandbox) claudeArgs(sessionID string, extra ...string) []string {
	args := []string{"--model", "claude-haiku-4-5", "--strict-mcp-config", "--no-chrome", "--disable-slash-commands",
		"--system-prompt", "Answer briefly and do exactly what is asked.", "--session-id", sessionID}
	if sb.Mode == RealLogin {
		args = append(args, "--settings", sb.ClaudeConfig+"/settings.json", "--setting-sources", "project")
	}
	return append(args, extra...)
}

// ClaudeInteractive runs one interactive session: start, get past any dialog
// a fresh config meets, send the prompt, wait for the reply, give the status
// line and the exporter a moment, then exit. It fails the test on anything
// unexpected rather than guessing its way through a screen.
func (sb *Sandbox) ClaudeInteractive(route Route, prompt string, reply *regexp.Regexp, extra ...string) *ClaudeRun {
	return sb.ClaudeInteractiveTurns(route, []string{prompt}, []*regexp.Regexp{reply}, nil, extra...)
}

// ClaudeInteractiveTurns can check delivery while the session is still open.
func (sb *Sandbox) ClaudeInteractiveTurns(route Route, prompts []string, replies []*regexp.Regexp, afterTurn func(int, string), extra ...string) *ClaudeRun {
	t := sb.T
	t.Helper()
	sb.connectClaude()
	sessionID := uuid.NewString()
	term, err := Start(sb.Repo, sb.claudeEnv(route), 40, 120, sb.Claude.Path, sb.claudeArgs(sessionID, extra...)...)
	if err != nil {
		t.Fatalf("start claude: %v", err)
	}
	run := &ClaudeRun{SessionID: sessionID, Route: route, Term: term}
	t.Cleanup(func() { _ = term.Close("", 2*time.Second) })
	defer func() {
		run.Text += "\n" + term.Text()
		run.Raw = term.Raw()
	}()

	// Dialogs a fresh config directory may show, each answered once. Dialogs are
	// checked before the prompt because their option lists use the same cursor.
	deadline := time.Now().Add(90 * time.Second)
	for {
		i, err := term.ExpectAny(time.Until(deadline), loginRE, trustRE, themeRE, bypassRE, promptRE)
		if err != nil {
			t.Fatalf("claude never showed a prompt:\n%s", tail(term.Text(), 3000))
		}
		switch i {
		case 0:
			t.Fatalf("claude asked to log in: no usable credential for route %s\n%s", route, tail(term.Text(), 1500))
		case 1:
			// The trust dialog defaults to "No, exit"; move to "Yes" first.
			time.Sleep(500 * time.Millisecond)
			if yesSecondRE.MatchString(term.TextSince()) {
				_ = term.Send("\x1b[B")
				time.Sleep(300 * time.Millisecond)
			}
			term.Consume()
			_ = term.Send("\r")
		case 2, 3:
			term.Consume()
			_ = term.Send("\r")
		case 4:
			goto ready
		}
	}
ready:
	time.Sleep(1500 * time.Millisecond) // the start-of-session status line render
	for turn, prompt := range prompts {
		term.Consume()
		if err := term.Type(prompt); err != nil {
			t.Fatal(err)
		}
		if _, err := term.Expect(replies[turn], 90*time.Second); err != nil {
			t.Fatalf("%v\n%s", err, tail(term.Text(), 3000))
		}
		// Give the status-line renderer and periodic OTel exporter time to run.
		time.Sleep(7 * time.Second)
		if afterTurn != nil {
			afterTurn(turn, sessionID)
		}
	}
	// Preserve the rendered screen before exit clears it; raw cursor commands
	// alone do not retain the status line's position at the start of its row.
	run.Text = term.Text()
	_ = term.Send("\x03")
	time.Sleep(400 * time.Millisecond)
	_ = term.Send("\x03")
	if err := term.Close("", 20*time.Second); err != nil {
		t.Logf("claude exit: %v", err)
	}
	return run
}

// ClaudeHeadless runs `claude -p` and returns its JSON result.
func (sb *Sandbox) ClaudeHeadless(route Route, prompt string, extra ...string) (map[string]any, string) {
	t := sb.T
	t.Helper()
	sb.connectClaude()
	sessionID := uuid.NewString()
	args := append([]string{"-p", prompt, "--output-format", "json", "--max-turns", "1", "--max-budget-usd", "0.05"},
		sb.claudeArgs(sessionID, extra...)...)
	cmd := exec.Command(sb.Claude.Path, args...)
	cmd.Dir = sb.Repo
	cmd.Env = sb.claudeEnv(route)
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("claude -p: %v\n%s\n%s", err, out, stderr)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("claude -p output is not JSON: %v\n%s", err, out)
	}
	return result, sessionID
}

// APIRequests are the receiver's Claude api_request records for a session.
func (sb *Sandbox) APIRequests(sessionID string, timeout time.Duration) []LogRecord {
	return sb.Receiver.WaitLogs(timeout, func(l LogRecord) bool {
		return l.Attrs["event.name"] == "api_request" && l.Attrs["session.id"] == sessionID
	})
}

// SpendOf sums the harness's own cost estimate over records.
func SpendOf(recs []LogRecord) float64 {
	var total float64
	for _, r := range recs {
		if v, err := strconv.ParseFloat(r.Attrs["cost_usd"], 64); err == nil {
			total += v
		}
	}
	return total
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// keysOf turns an event's attribute map into the flat string map the golden
// check reads.
func keysOf(attrs map[string]any) map[string]string {
	out := make(map[string]string, len(attrs))
	for k, v := range attrs {
		out[k] = fmt.Sprint(v)
	}
	return out
}

func containsAll(s string, subs ...string) []string {
	var missing []string
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			missing = append(missing, sub)
		}
	}
	return missing
}
