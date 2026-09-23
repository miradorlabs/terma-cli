package live

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Codex is driven through `codex exec`, its non-interactive mode: it runs the
// same hooks, exports the same telemetry and writes the same rollout as the
// TUI, and prints its events as JSONL. Hook trust is a per-developer act made
// inside the TUI, so automation passes --dangerously-bypass-hook-trust, the
// flag Codex provides for exactly this; the interactive trust flow is a
// separate contract for later.
//
// Routes: the ChatGPT login is a file, $CODEX_HOME/auth.json, so the
// subscription route copies the developer's own into the scratch home (with
// their agreement: a refresh there rotates tokens the real file also holds);
// the API route logs in with the supplied key over stdin into scratch file
// storage. OPENAI_API_KEY alone does not authenticate the built-in provider.

// CodexCreds is what the environment offers for Codex.
type CodexCreds struct {
	APIKey string // OPENAI_API_KEY
	// AuthFile is the developer's ChatGPT login to copy for the subscription route.
	AuthFile string
}

// CodexCredentials reads the credential environment. TERMA_LIVE_CODEX_AUTH
// names an auth.json to copy; the default is the developer's own when
// TERMA_LIVE_REAL_LOGIN=1.
func CodexCredentials() CodexCreds {
	c := CodexCreds{APIKey: os.Getenv("OPENAI_API_KEY"), AuthFile: os.Getenv("TERMA_LIVE_CODEX_AUTH")}
	if c.AuthFile == "" && os.Getenv("TERMA_LIVE_REAL_LOGIN") == "1" {
		home, _ := os.UserHomeDir()
		if p := filepath.Join(home, ".codex", "auth.json"); fileExists(p) {
			c.AuthFile = p
		}
	}
	return c
}

// tomlQuote is a basic TOML string for a table key.
func tomlQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// CodexRun is one finished `codex exec`.
type CodexRun struct {
	ThreadID string
	Route    Route
	Events   []map[string]any
	Stdout   string
	Stderr   string
}

func (sb *Sandbox) codexEnv(route Route) []string {
	env := append(sb.baseEnv(), "TERMA_CONFIG_DIR="+sb.TermaConfig, "CODEX_HOME="+sb.CodexHome)
	if route == RouteAPIKey {
		env = append(env, "OPENAI_API_KEY="+CodexCredentials().APIKey)
	}
	return env
}

// prepareCodex trusts the sandbox repository, connects Codex to the receiver
// and installs the route's login. Trust first: an untrusted project skips its
// .codex/ layer, hooks included, and the record is what a developer's own
// approval writes into config.toml.
func (sb *Sandbox) prepareCodex(route Route) {
	t := sb.T
	t.Helper()
	if !sb.codexConnected {
		cfg := filepath.Join(sb.CodexHome, "config.toml")
		trust := "[projects." + tomlQuote(sb.Repo) + "]\ntrust_level = \"trusted\"\n"
		existing, _ := os.ReadFile(cfg)
		if err := os.WriteFile(cfg, append(existing, []byte("\n"+trust)...), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sb.connectCodex()
	if route == RouteAPIKey {
		// Use Codex's login flow, scoped to the scratch CODEX_HOME. Keep the
		// key off command arguments and force file storage to avoid Keychain.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, sb.Codex.Path, "login", "--with-api-key", "-c", `cli_auth_credentials_store="file"`, "-c", "features.plugins=false", "-c", "features.remote_plugin=false")
		cmd.Dir = sb.Repo
		cmd.Env = sb.codexEnv(route)
		cmd.Stdin = strings.NewReader(CodexCredentials().APIKey + "\n")
		if err := cmd.Run(); err != nil {
			t.Fatalf("codex API-key login in scratch profile: %v", err)
		}
	}
	if route == RouteSubscription {
		src := CodexCredentials().AuthFile
		if src == "" {
			t.Fatal("no Codex login to copy")
		}
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sb.CodexHome, "auth.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// CodexExec runs one non-interactive turn in the sandbox repository.
func (sb *Sandbox) CodexExec(route Route, prompt string, extra ...string) *CodexRun {
	t := sb.T
	t.Helper()
	sb.prepareCodex(route)
	args := append([]string{"exec", "--json", "--skip-git-repo-check", "--dangerously-bypass-hook-trust",
		"-C", sb.Repo, "-c", `cli_auth_credentials_store="file"`,
		// Plugin catalog clones can outlive Codex and race TempDir cleanup.
		// These contracts exercise hooks/telemetry, not plugin installation.
		"-c", "features.plugins=false", "-c", "features.remote_plugin=false",
		"-c", `model_reasoning_effort="low"`}, extra...)
	args = append(args, prompt)
	cmd := exec.Command(sb.Codex.Path, args...)
	cmd.Dir = sb.Repo
	cmd.Env = sb.codexEnv(route)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Stdin = nil
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start codex: %v", err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("codex exec: %v\n%s\n%s", err, stdout.String(), stderr.String())
		}
	case <-time.After(scenarioTimeout):
		_ = cmd.Process.Kill()
		t.Fatalf("codex exec did not finish in %v\n%s\n%s", scenarioTimeout, stdout.String(), stderr.String())
	}
	run := &CodexRun{Route: route, Stdout: stdout.String(), Stderr: stderr.String()}
	sc := bufio.NewScanner(strings.NewReader(run.Stdout))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var ev map[string]any
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		run.Events = append(run.Events, ev)
		if ev["type"] == "thread.started" {
			run.ThreadID, _ = ev["thread_id"].(string)
		}
	}
	if run.ThreadID == "" {
		t.Fatalf("codex exec printed no thread.started:\n%s\n%s", run.Stdout, run.Stderr)
	}
	return run
}

// CodexLogs are the receiver's Codex records for a thread: the conversation
// start and every completed response.
func (sb *Sandbox) CodexLogs(threadID string, timeout time.Duration) (starts, completed []LogRecord) {
	all := sb.Receiver.WaitLogs(timeout, func(l LogRecord) bool {
		return l.Attrs["conversation.id"] == threadID && l.Attrs["event.name"] == "codex.sse_event" && l.Attrs["event.kind"] == "response.completed"
	})
	completed = all
	for _, l := range sb.Receiver.Logs() {
		if l.Attrs["conversation.id"] == threadID && l.Attrs["event.name"] == "codex.conversation_starts" {
			starts = append(starts, l)
		}
	}
	return starts, completed
}
