package e2e

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Grok Build (xAI's grok CLI) exports natively: its OTLP logs, which setup points at the
// relay through [telemetry] in $GROK_HOME/config.toml; terma's hooks file
// ($GROK_HOME/hooks/terma.json) claims each session and spools its prompts, subagents and
// edits. Every run here is in a scratch GROK_HOME inside the sandbox, with a custom model
// whose base_url is a local OpenAI-compatible stub: grok runs with no login and no key, and
// with remote_fetch off its export starts at once. The builds are the installed one, or
// TERMA_E2E_GROK_VERSIONS's, from npm (@xai-official/grok, whose platform binary is an
// optional dependency).

// grokPackage is grok's npm package, which lists its releases.
const grokPackage = "@xai-official/grok"

// grokModel is the custom model the stub serves, by the name grok's config gives it.
const grokModel = "fake-m"

func forEachGrok(t *testing.T, run func(t *testing.T, b Binary)) {
	t.Helper()
	if !Enabled() {
		t.Skip("live tests run only with TERMA_E2E=1")
	}
	skipUnlessSelected(t, "grok")
	spec := cmp.Or(os.Getenv("TERMA_E2E_GROK_VERSIONS"), "installed")
	builds := resolve(t, "grok", spec, func() ([]string, error) { return npmReleases(grokPackage) }, ensureGrok)
	if len(builds) == 0 {
		Record(t.Name(), "not run", "grok is not installed")
		t.Skip("grok is not installed")
	}
	for _, b := range builds {
		t.Run(b.Label(), func(t *testing.T) { scenarioOfBuild(t, b); run(t, b) })
	}
}

// ensureGrok installs a grok build from npm. Its postinstall links the binary into
// $GROK_HOME/bin and records the installer in $GROK_HOME/config.toml, so the install runs
// with a home of its own beside the build, never the developer's ~/.grok.
func ensureGrok(version string) (string, error) {
	if err := CheckPathName("grok version", version); err != nil {
		return "", err
	}
	home, err := filepath.Abs(filepath.Join(versionsDir(), "grok", version, "install-home"))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", err
	}
	return ensureNpmBuild("grok", grokPackage, "grok", "HOME="+home, "GROK_HOME="+filepath.Join(home, ".grok"))(version)
}

// grokHome is the sandbox's GROK_HOME.
func (sb *Sandbox) grokHome() string { return filepath.Join(sb.Home, ".grok") }

// UseGrok gives the sandbox's grok a home of its own, never the developer's ~/.grok, whose
// config.toml names the stub at url as its only model, with grok's remote settings fetch off.
// grok's compatibility hooks are off: the sandbox's Claude Code settings hold terma's Claude
// Code hooks, which stand down inside grok and would only add processes. It must run before
// setup, which adds [telemetry] to this file.
func (sb *Sandbox) UseGrok(url string) {
	sb.ExtraEnv = append(sb.ExtraEnv, "GROK_HOME="+sb.grokHome(), "GROK_CLAUDE_HOOKS_ENABLED=0", "GROK_CURSOR_HOOKS_ENABLED=0",
		"GROK_CLAUDE_MCPS_ENABLED=0", "GROK_CURSOR_MCPS_ENABLED=0", "GROK_DISABLE_AUTOUPDATER=1")
	sb.writeAbs(filepath.Join(sb.grokHome(), "config.toml"), `[models]
default = "`+grokModel+`"

[model.`+grokModel+`]
model = "`+grokModel+`"
base_url = "`+url+`/v1"
api_key = "synthetic"
context_window = 128000

[features]
remote_fetch = false
`)
}

// GrokRun runs one `grok -p` in dir, approving every tool.
func (sb *Sandbox) GrokRun(b Binary, dir, prompt string) string {
	t := sb.T
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.Path, "-p", prompt, "-m", grokModel, "--always-approve", "--output-format", "json")
	cmd.Dir = dir
	cmd.Env = sb.termaEnv()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("grok -p: %v\n%s", err, out.String())
	}
	return out.String()
}

// GrokACP drives `grok agent stdio` in dir as an editor does over ACP (JSON-RPC, one message a
// line): it opens a session, or loads session when given (as T3 Code does for each turn after
// the first, in a new process), sends each prompt in turn, naming it with _meta.promptId when
// promptID is set (as T3 Code does), approves any permission grok asks for, and returns the
// session's id.
func (sb *Sandbox) GrokACP(b Binary, dir, session, promptID string, prompts ...string) string {
	t := sb.T
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.Path, "agent", "stdio")
	cmd.Dir = dir
	cmd.Env = sb.termaEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("grok agent stdio: %v", err)
	}
	enc, lines := json.NewEncoder(stdin), bufio.NewScanner(stdout)
	lines.Buffer(nil, 4<<20)
	// call sends one request and reads until its response, answering grok's own requests.
	call := func(id int, method string, params any) json.RawMessage {
		t.Helper()
		if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
			t.Fatalf("%s: %v\n%s", method, err, stderr.String())
		}
		for lines.Scan() {
			var m struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params struct {
					Options []struct {
						OptionID string `json:"optionId"`
						Kind     string `json:"kind"`
					} `json:"options"`
				} `json:"params"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(lines.Bytes(), &m) != nil || len(m.ID) == 0 {
				continue
			}
			if m.Method != "" {
				option := ""
				for _, o := range m.Params.Options {
					if option == "" || strings.Contains(o.Kind, "allow") {
						option = o.OptionID
					}
				}
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": option}}})
				continue
			}
			if string(m.ID) != strconv.Itoa(id) {
				continue
			}
			if len(m.Error) > 0 {
				t.Fatalf("%s: %s\n%s", method, m.Error, stderr.String())
			}
			return m.Result
		}
		t.Fatalf("%s: grok closed its output: %v\n%s", method, lines.Err(), stderr.String())
		return nil
	}
	call(1, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{
		"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false}})
	if session == "" {
		var opened struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(call(2, "session/new", map[string]any{"cwd": dir, "mcpServers": []any{}}), &opened); err != nil || opened.SessionID == "" {
			t.Fatalf("session/new gave no session: %v", err)
		}
		session = opened.SessionID
	} else {
		call(2, "session/load", map[string]any{"sessionId": session, "cwd": dir, "mcpServers": []any{}})
	}
	for i, prompt := range prompts {
		params := map[string]any{"sessionId": session, "prompt": []any{map[string]any{"type": "text", "text": prompt}}}
		if promptID != "" {
			params["_meta"] = map[string]any{"promptId": promptID}
		}
		call(10+i, "session/prompt", params)
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("grok agent stdio: %v\n%s", err, stderr.String())
	}
	return session
}
