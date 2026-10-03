// Package live drives the real coding agents, with real provider credentials,
// through the real terma binary, and checks that what Terma collects is what
// the harness × information matrix says it collects. It exists because no one
// can keep re-verifying four harnesses by hand every release: a cell in the
// matrix is either a passing test here or a claim.
//
// Every test runs in a sandbox: a scratch Terma config directory, a scratch
// harness config directory, and a scratch git repository that `terma install`
// has configured exactly as it would for a developer, with the harness's export
// pointed at an OTLP receiver inside the test. The contracts then read three planes
// together: the hook spool, the receiver, and the terminal.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// exec_LookPath is exec.LookPath under a name the versions code can use without
// colliding with the package's own exec imports.
var exec_LookPath = exec.LookPath

// Mode is where the harness's login comes from.
type Mode int

const (
	// Isolated: a fresh harness config directory; credentials come from the
	// environment (CLAUDE_CODE_OAUTH_TOKEN, ANTHROPIC_API_KEY, OPENAI_API_KEY).
	// This is the CI shape and the only one that exercises the wrapped status
	// line end to end.
	Isolated Mode = iota
	// RealLogin: the developer's own Claude config directory and Keychain login,
	// with Terma's scratch settings passed through --settings. Costs the same
	// prompt on the developer's plan; skips the contracts that need the scratch
	// config directory to be the one Claude reads.
	RealLogin
)

// Sandbox is one test's isolated world.
type Sandbox struct {
	account      *Account
	T            *testing.T
	Mode         Mode
	Dir          string
	Home         string
	TermaConfig  string
	ClaudeConfig string
	CodexHome    string
	Repo         string
	Terma        string
	Receiver     *Receiver
	// ProjectID is the Terma project the scratch repository is bound to.
	ProjectID string
	// RendererMarker is what the pre-existing status line prints; the wrapped
	// status line must still show it.
	RendererMarker string
	// Claude and Codex are the builds under test ("claude"/"codex" on the PATH when unset).
	Claude Binary
	Codex  Binary
	// ClaudeBaseURL is set only by scenarios using a synthetic loopback provider.
	ClaudeBaseURL string
	// ExcludeContent is a team policy that withholds prompts and tool content, applied by
	// the relay: no exporter has a switch for it.
	ExcludeContent bool

	claudeDirect, codexDirect bool
	// WorkDir is where an agent run starts; the sandbox repository when empty. The
	// relay's negative controls run agents outside the installed repository.
	WorkDir string
	// CodexHooksUntrusted runs Codex without bypassing its hook trust, as a developer
	// who has not yet trusted the project's hooks does.
	CodexHooksUntrusted bool
	// ExtraEnv is appended to every environment the sandbox builds, so a setting
	// reaches the agents and the hooks and relays they start.
	ExtraEnv []string
	// RelayAgents are pointed at the relay too, beyond Claude Code, Codex and OpenCode
	// (UseRelay).
	RelayAgents []string
	// relayed means the agents export to the local relay (UseRelay), never straight to
	// the receiver: the scenario is that nothing else reaches it.
	relayed      bool
	relayAddr    string
	codexTrusted map[string]bool
	// codexTrustWithdrawn counts the approvals CodexHooksUntrusted took back.
	codexTrustWithdrawn int
}

// Option adjusts a sandbox before it is configured.
type Option func(*Sandbox)

// WithClaude runs the scenario against this Claude Code build.
func WithClaude(b Binary) Option { return func(sb *Sandbox) { sb.Claude = b } }

// WithCodex runs the scenario against this Codex build.
func WithCodex(b Binary) Option { return func(sb *Sandbox) { sb.Codex = b } }

const (
	// liveKey is the server key the agents and the spool export with: a syntactically
	// valid key the receiver accepts without checking. Nothing mints anything.
	liveKey = "ter_srv_00000000000000000000000000000000"
)

// Enabled gates every live test on an explicit opt-in.
func Enabled() bool { return os.Getenv("TERMA_E2E") == "1" }

// New builds a sandbox and configures it through terma.
func New(t *testing.T, mode Mode, opts ...Option) *Sandbox {
	t.Helper()
	if !Enabled() {
		t.Skip("live tests run only with TERMA_E2E=1")
	}
	terma := os.Getenv("TERMA_E2E_BINARY")
	if terma == "" {
		terma = "terma"
	}
	if p, err := exec.LookPath(terma); err == nil {
		terma = p
	} else {
		t.Fatalf("terma binary not found (%s); run make build", terma)
	}
	dir := t.TempDir()
	// macOS gives /var/folders/... which is a symlink of /private/var/...; the
	// harness reports real paths, so resolve once and use the real one throughout.
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	sb := &Sandbox{T: t, Mode: mode, Dir: dir, Terma: terma, ProjectID: "proj_live",
		Home: filepath.Join(dir, "home"), TermaConfig: filepath.Join(dir, "terma"),
		ClaudeConfig: filepath.Join(dir, "claude"), CodexHome: filepath.Join(dir, "codex"),
		Repo: filepath.Join(dir, "repo"), RendererMarker: "LIVE-RENDERER",
		Claude: Binary{Harness: "claude", Path: "claude"}, Codex: Binary{Harness: "codex", Path: "codex"}}
	for _, o := range opts {
		o(sb)
	}
	for _, d := range []string{sb.Home, sb.TermaConfig, sb.ClaudeConfig, sb.CodexHome, sb.Repo} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sb.Receiver = StartReceiver(t)

	sb.git("init", "-q", "-b", "main")
	sb.git("config", "user.email", "live@terma.test")
	sb.git("config", "user.name", "Terma Live")
	sb.git("config", "commit.gpgsign", "false")
	sb.write("README.md", "live sandbox\n")
	sb.git("add", "README.md")
	sb.git("commit", "-q", "-m", "init")

	// A status line the developer already had. printf ignores stdin, which is
	// its own test of the wrapper.
	renderer := filepath.Join(dir, "renderer.sh")
	sb.writeAbs(renderer, "#!/bin/sh\nprintf '"+sb.RendererMarker+" %s' \"${COLUMNS:-0}\"\n")
	_ = os.Chmod(renderer, 0o755)
	settings := map[string]any{"statusLine": map[string]any{"type": "command", "command": renderer, "padding": 0}}
	raw, _ := json.MarshalIndent(settings, "", "  ")
	sb.writeAbs(filepath.Join(sb.ClaudeConfig, "settings.json"), string(raw)+"\n")
	// Skip onboarding and trust in the scratch config; the developer's real one
	// has long since done both.
	state := map[string]any{"hasCompletedOnboarding": true, "theme": "dark", "numStartups": 5,
		"projects": map[string]any{sb.Repo: map[string]any{"hasTrustDialogAccepted": true, "allowedTools": []string{}}}}
	raw, _ = json.MarshalIndent(state, "", "  ")
	sb.writeAbs(filepath.Join(sb.ClaudeConfig, ".claude.json"), string(raw)+"\n")

	// The real terma paths: the profile's ingest URL is the receiver, so the
	// background flushes the hooks start deliver there; the repository is installed
	// with both adapters' hooks. The account fixture provides the developer login,
	// and the collection policy the relay and the spool's delivery fetch with it.
	// Providers remain local fixtures.
	sb.terma(sb.Repo, "config", "set", "--otlp-url", sb.Receiver.URL())
	sb.StartAccount()
	// Each scenario points its own exporter. --no-browser bounds a fixture-login
	// regression instead of opening a browser.
	sb.terma(sb.Repo, "install", "--team", sb.ProjectID, "--harness", "none", "--adapters", "claude,codex", "--yes", "--no-browser")
	return sb
}

// Delivered waits for Terma's own flush to have delivered a spooled event of
// that name for the session to the receiver: the shape the backend parses,
// not the spool file. Returns the matching records.
func (sb *Sandbox) Delivered(name, session string, timeout time.Duration) []LogRecord {
	return sb.Receiver.WaitLogs(timeout, func(l LogRecord) bool {
		return l.Resource["service.name"] == "terma-cli" && l.Attrs["event.name"] == name &&
			(session == "" || l.Attrs["session.id"] == session)
	})
}

// useLiveKey files liveKey as the project's key, which the spool's flushes deliver with,
// in place of the one install had the account fixture mint.
func (sb *Sandbox) useLiveKey() {
	sb.T.Helper()
	path := filepath.Join(sb.TermaConfig, "keys.json")
	doc := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &doc)
	}
	keys, _ := doc["keys"].(map[string]any)
	if keys == nil {
		keys = map[string]any{}
	}
	keys[sb.ProjectID] = liveKey
	doc["keys"] = keys
	data, _ := json.MarshalIndent(doc, "", "  ")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		sb.T.Fatal(err)
	}
}

// RouteClaude points Claude Code at this repository's project the way `terma install`
// does for a developer: its user-level exporter at the local relay, which forwards the
// sessions this repository's hooks claim to the receiver, standing in for Terma. With
// ExcludeContent the team's policy, which the account fixture serves, withholds prompts
// and tool content; install has no switch for them. The account fixture supplies the
// developer login the relay fetches that policy with. UseRelay files the receiver's key,
// so install mints none.
func (sb *Sandbox) RouteClaude() {
	sb.T.Helper()
	if sb.Mode != Isolated {
		sb.T.Fatal("RouteClaude needs an isolated sandbox")
	}
	// The relay on a port of its own, forwarding to the receiver; install finds it there.
	sb.UseRelay(RelayOptions{Start: true, Content: !sb.ExcludeContent})
	sb.terma(sb.Repo, "install", "--team", sb.ProjectID, "--harness", "claude", "--yes", "--no-browser")
	// What install wrote is the only exporter: the relay's, in the user's settings.
	if user, err := os.ReadFile(filepath.Join(sb.ClaudeConfig, "settings.json")); err != nil || !bytes.Contains(user, []byte(sb.relayAddr)) {
		sb.T.Fatalf("install did not point Claude Code at the relay (%s): %v\n%s", sb.relayAddr, err, user)
	}
}

// workDir is where an agent run starts.
func (sb *Sandbox) workDir() string {
	if sb.WorkDir != "" {
		return sb.WorkDir
	}
	return sb.Repo
}

// claudeLauncher is what a run starts: the build under test.
func (sb *Sandbox) claudeLauncher() string {
	return sb.Claude.Path
}

// termaEnv is the environment terma itself runs with: its scratch config and
// the scratch harness config, so it writes there and nowhere else.
func (sb *Sandbox) termaEnv() []string {
	return append(sb.baseEnv(), "TERMA_CONFIG_DIR="+sb.TermaConfig, "CLAUDE_CONFIG_DIR="+sb.ClaudeConfig, "CODEX_HOME="+sb.CodexHome)
}

// baseEnv is the environment harnesses and terma run with.
//
// Isolated mode builds a clean one: system paths plus the terma binary's
// directory, a terminal, and nothing of the developer's own agent settings, so
// the only credential is the one the scenario sets. Real-login mode inherits
// the developer's environment instead, because the macOS Keychain that holds
// the Claude login is reached through the security session that environment
// describes; only the agent variables are removed, so the harness under test
// cannot tell it was started from inside another agent.
func (sb *Sandbox) baseEnv() []string {
	// The build under test first: terma's own detection and any `claude` a script
	// runs must both mean this one.
	path := sb.binDir() + filepath.Dir(sb.Terma) + ":/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	for p := range strings.SplitSeq(os.Getenv("PATH"), ":") {
		if p != "" && !strings.Contains(path, p) {
			path += ":" + p
		}
	}
	var env []string
	if sb.Mode == RealLogin {
		for _, kv := range os.Environ() {
			key, _, _ := strings.Cut(kv, "=")
			switch {
			case key == "CLAUDECODE", strings.HasPrefix(key, "CLAUDE_CODE_"), key == "CLAUDE_CONFIG_DIR",
				strings.HasPrefix(key, "ANTHROPIC_"), strings.HasPrefix(key, "TERMA_"), strings.HasPrefix(key, "OTEL_"),
				key == "PATH", key == "TERM", key == "COLORTERM":
				continue
			}
			env = append(env, kv)
		}
	} else {
		env = []string{"HOME=" + sb.Home, "LANG=en_US.UTF-8", "TMPDIR=" + os.TempDir()}
		if runtime.GOOS == "darwin" {
			env = append(env, "SHELL=/bin/zsh")
		}
	}
	// TERMA_RELAY_SERVICE=0: `terma install` in a sandbox must not register the
	// sandbox's relay with the developer's service manager; each test runs its own.
	env = append(env, "PATH="+path, "TERM=xterm-256color", "COLORTERM=truecolor", "TERMA_RELAY_SERVICE=0")
	// The one terma setting carried across: which environment's auth and API hosts a
	// run may reach (TERMA_ENV=dev keeps anything that is not the receiver off
	// production). Everything else of the developer's terma stays out.
	if v := os.Getenv("TERMA_ENV"); v != "" {
		env = append(env, "TERMA_ENV="+v)
	}
	return append(env, sb.ExtraEnv...)
}

// binDir is a PATH prefix holding the builds under test under their plain
// names, so `claude` and `codex` resolve to them, and a recording `terma`: a
// shim that copies each hook's stdin to a file before running the real binary,
// so the payload every harness version sends is evidence the suite keeps.
func (sb *Sandbox) binDir() string {
	dir := filepath.Join(sb.Dir, "path")
	if _, err := os.Stat(dir); err != nil {
		_ = os.MkdirAll(dir, 0o755)
		_ = os.MkdirAll(sb.payloadDir(), 0o755)
		for _, b := range []Binary{sb.Claude, sb.Codex} {
			if b.Path != "" && filepath.IsAbs(b.Path) {
				_ = os.Symlink(b.Path, filepath.Join(dir, b.Harness))
			}
		}
		shim := "#!/bin/sh\n" +
			"# Records a hook's stdin, then runs terma with it. Written by the live suite.\n" +
			"if [ \"$1\" = hook ]; then\n" +
			"  f=\"" + sb.payloadDir() + "/$(date +%s%N)-$2.json\"\n" +
			"  cat > \"$f\"\n" +
			"  exec \"" + sb.Terma + "\" \"$@\" < \"$f\"\n" +
			"fi\n" +
			"exec \"" + sb.Terma + "\" \"$@\"\n"
		_ = os.WriteFile(filepath.Join(dir, "terma"), []byte(shim), 0o755)
	}
	return dir + ":"
}

func (sb *Sandbox) payloadDir() string { return filepath.Join(sb.Dir, "hook-payloads") }

// HookPayloads returns what the harness wrote to a hook's stdin, per
// invocation of that hook event, in order.
func (sb *Sandbox) HookPayloads(event string) []map[string]any {
	matches, _ := filepath.Glob(filepath.Join(sb.payloadDir(), "*-"+event+".json"))
	var out []map[string]any
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil || len(bytes.TrimSpace(data)) == 0 {
			continue
		}
		var v map[string]any
		if json.Unmarshal(data, &v) == nil {
			out = append(out, v)
		}
	}
	return out
}

func (sb *Sandbox) terma(dir string, args ...string) string {
	sb.T.Helper()
	return sb.termaWith(nil, dir, args...)
}

// termaWith is terma with extra environment for this one command.
func (sb *Sandbox) termaWith(env []string, dir string, args ...string) string {
	sb.T.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, sb.Terma, args...)
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = dir
	cmd.Env = append(sb.termaEnv(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		sb.T.Fatalf("terma %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (sb *Sandbox) git(args ...string) string {
	sb.T.Helper()
	return sb.gitIn(sb.Repo, args...)
}

// gitIn runs git in dir with the sandbox's environment.
func (sb *Sandbox) gitIn(dir string, args ...string) string {
	sb.T.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(sb.baseEnv(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		sb.T.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (sb *Sandbox) write(rel, content string) { sb.writeAbs(filepath.Join(sb.Repo, rel), content) }

func (sb *Sandbox) writeAbs(path, content string) {
	sb.T.Helper()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		sb.T.Fatal(err)
	}
}

// Commit stages everything and commits, returning the full commit message, so
// a contract can look for the Agent-Session-Id trailer the hooks stamp.
func (sb *Sandbox) Commit(message string) string {
	sb.T.Helper()
	sb.git("add", "-A")
	cmd := exec.Command("git", "commit", "-q", "-m", message)
	cmd.Dir = sb.Repo
	cmd.Env = append(sb.baseEnv(), "TERMA_CONFIG_DIR="+sb.TermaConfig, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		sb.T.Fatalf("git commit: %v\n%s", err, out)
	}
	return sb.git("log", "-1", "--format=%B")
}

// Version runs `<binary> --version` and returns the first line.
func Version(binary string) string {
	out, err := exec.CommandContext(context.Background(), binary, "--version").Output()
	if err != nil {
		return "not installed"
	}
	line, _, _ := bytes.Cut(bytes.TrimSpace(out), []byte("\n"))
	return string(line)
}

// timeout for one harness scenario end to end.
const scenarioTimeout = 3 * time.Minute

func fatalf(t *testing.T, format string, a ...any) {
	t.Helper()
	t.Fatalf(format, a...)
}

var _ = fmt.Sprintf
