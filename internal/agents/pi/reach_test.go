package pi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

func TestPiStartRunAndToolReachCrossOrgMixed(t *testing.T) {
	stateDir := t.TempDir()
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	repoA, repoB := filepath.Join(parent, "alpha"), filepath.Join(parent, "beta")
	for _, r := range [][2]string{{repoA, "git@github.com:orgA/alpha.git"}, {repoB, "git@github.com:orgB/beta.git"}} {
		_, _ = gitx.Git(context.Background(), parent, "init", "-q", "-b", "main", r[0])
		_, _ = gitx.Git(context.Background(), r[0], "remote", "add", "origin", r[1])
	}
	fileB := filepath.Join(repoB, "secret.go")
	if err := os.WriteFile(fileB, []byte("package b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const acct = "1111111111111111"
	now := time.Now().UTC().Truncate(time.Second)
	st, err := routing.Create(stateDir, "default", acct, func(s *routing.State) error {
		for _, spec := range [][3]string{{"orgA", "team-a", "github.com/orgA/alpha"}, {"orgB", "team-b", "github.com/orgB/beta"}} {
			if _, err := s.Select(routing.Team{OrganizationID: spec[0], AuthURL: "https://api.example.com", TeamID: spec[1],
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{spec[2]}, Revision: 1, FetchedAt: now, TeamID: spec[1], OrganizationID: spec[0], AuthURL: "https://api.example.com"}}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spool.Open(filepath.Join(stateDir, spool.Dir))
	if err != nil {
		t.Fatal(err)
	}
	sp.Bind(acct)
	rt := hookrun.NewRouting("default", &st)
	env := func(stdin string) hookrun.Env {
		return hookrun.Env{StateDir: stateDir, Cwd: repoA, Spool: sp, Account: acct, Routing: rt, Now: now, Stdin: strings.NewReader(stdin)}
	}
	events := Agent{}.Events()
	ctx := context.Background()

	parentID := "018f3a2c-9201-7000-8000-000000000001"
	childID := "018f3a2c-9201-7000-8000-000000000002"
	qA := strconv.Quote(repoA)
	if err := events["pi-session-start"](ctx, env(`{"session_id":"`+parentID+`","cwd":`+qA+`}`)); err != nil {
		t.Fatal(err)
	}
	parentRun, ok := runs.Locate(stateDir, parentID)
	if !ok || parentRun.Run == "" {
		t.Fatalf("pi-session-start did not start run: %+v (ok=%v)", parentRun, ok)
	}
	if err := events["pi-session-start"](ctx, env(`{"session_id":"`+childID+`","cwd":`+qA+`,"parent_session_id":"`+parentID+`"}`)); err != nil {
		t.Fatal(err)
	}
	if p, ok := runs.Locate(stateDir, childID); !ok || p.Run != parentRun.Run {
		t.Fatalf("subagent pi-session-start run = %q (ok=%v); want parent run %q", p.Run, ok, parentRun.Run)
	}

	reaches := []map[string]any{
		{"tool": "read", "paths": []string{fileB}},
		{"tool": "grep", "paths": []string{repoB}, "globs": []string{"*.go"}},
		{"tool": "find", "globs": []string{"../beta/**/*.go"}},
		{"tool": "bash", "command": "cat ../beta/secret.go"},
	}
	for i, payload := range reaches {
		sid := "018f3a2c-9202-7000-8000-00000000000" + string(rune('1'+i))
		if err := events["pi-session-start"](ctx, env(`{"session_id":"`+sid+`","cwd":`+qA+`}`)); err != nil {
			t.Fatal(err)
		}
		payload["session_id"] = sid
		payload["cwd"] = repoA
		body, _ := json.Marshal(payload)
		if err := events["pi-tool-reach"](ctx, env(string(body))); err != nil {
			t.Fatal(err)
		}
		if c, ok := claim.Read(stateDir, sid, now); !ok || c.Kind != claim.KindMixed {
			t.Fatalf("%v: claim = %+v (ok=%v); want KindMixed", payload["tool"], c, ok)
		}
	}
}

func TestPiRefreshMachineUpdatesInstalledExtension(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", dir)
	path := filepath.Join(dir, "extensions", "terma.ts")

	if got, changed, err := (Agent{}).RefreshMachine(); err != nil || changed || got != path {
		t.Fatalf("RefreshMachine(absent) = (%q, %v, %v); want (%q, false, nil)", got, changed, err, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	old := `// Terma old pi extension
const CONFIG: TermaConfig | null = {"version":1,"endpoint":"http://127.0.0.1:43180","headers":{"Authorization":"Bearer tok"},"includePrompts":true,"includeToolContent":true,"hookCommand":["/custom/terma","hook"]} /* terma:config */
export default function () {}
`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := (Agent{}).RefreshMachine(); err != nil || !changed {
		t.Fatalf("RefreshMachine(outdated) = (%v, %v); want (true, nil)", changed, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"/custom/terma"`, `"http://127.0.0.1:43180"`, `"tool-reach"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("refreshed extension missing %s", want)
		}
	}
	if _, changed, err := (Agent{}).RefreshMachine(); err != nil || changed {
		t.Fatalf("RefreshMachine(current) = (%v, %v); want (false, nil)", changed, err)
	}
}

// A failed reach check fences the session: nothing more of it is exported until a new run
// starts it again; and a tool span ends only after its reach check has answered.
func TestPiToolReachFailsClosedUnderNode(t *testing.T) {
	node := nodeForTest(t)
	dir := t.TempDir()
	reachDoneMarker := filepath.Join(dir, "reach.done")

	type seenSpan struct {
		Name    string
		EndNano int64
		Session string
	}
	var mu sync.Mutex
	var spans []seenSpan
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/v1/traces" {
			var req struct {
				ResourceSpans []struct {
					ScopeSpans []struct {
						Spans []struct {
							Name            string `json:"name"`
							EndTimeUnixNano string `json:"endTimeUnixNano"`
							Attributes      []struct {
								Key   string `json:"key"`
								Value struct {
									StringValue string `json:"stringValue"`
								} `json:"value"`
							} `json:"attributes"`
						} `json:"spans"`
					} `json:"scopeSpans"`
				} `json:"resourceSpans"`
			}
			if json.Unmarshal(body, &req) == nil {
				mu.Lock()
				for _, rs := range req.ResourceSpans {
					for _, ss := range rs.ScopeSpans {
						for _, sp := range ss.Spans {
							sid := ""
							for _, a := range sp.Attributes {
								if a.Key == "session.id" {
									sid = a.Value.StringValue
								}
							}
							endNano, _ := strconv.ParseInt(sp.EndTimeUnixNano, 10, 64)
							spans = append(spans, seenSpan{Name: sp.Name, EndNano: endNano, Session: sid})
						}
					}
				}
				mu.Unlock()
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hook := filepath.Join(dir, "hook.sh")
	hookSrc := "#!/bin/sh\n" +
		"payload=$(cat)\n" +
		"if [ \"$1\" = \"pi-tool-reach\" ]; then\n" +
		"  case \"$payload\" in\n" +
		"    *TERMA_REACH_FAILED*) exit 0 ;;\n" +
		"    *'\"session_id\":\"s-fail\"'*) exit 1 ;;\n" +
		"    *'\"session_id\":\"s-slow\"'*) sleep 0.15; touch " + strconv.Quote(reachDoneMarker) + " ;;\n" +
		"  esac\n" +
		"fi\n"
	if err := os.WriteFile(hook, []byte(hookSrc), 0o700); err != nil {
		t.Fatal(err)
	}
	ext := filepath.Join(dir, "terma.ts")
	if _, err := writeExtension(ext, relayExtension(agents.RelayConfig{Endpoint: srv.URL, Token: "relay-token", HookCommand: []string{hook}})); err != nil {
		t.Fatal(err)
	}
	host := filepath.Join(dir, "node_modules", "@earendil-works", "pi-coding-agent")
	if err := os.MkdirAll(host, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host, "package.json"), []byte(`{"name":"@earendil-works/pi-coding-agent","type":"module","main":"index.js"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host, "index.js"), []byte("export const VERSION = \"1.6.1\"\nexport const CONFIG_DIR_NAME = \".pi\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	driver := `
import { pathToFileURL } from "node:url"
const mod = await import(pathToFileURL(process.argv[2]).href)
const handlers = {}
await mod.default({ on: (name, fn) => { handlers[name] = fn } })
const sm = (id) => ({ getSessionId: () => id, getEntries: () => [], getEntry: () => undefined, getLeafId: () => null, getHeader: () => null })
const ctx = (id) => ({ cwd: "/repoA", mode: "print", model: { provider: "fake", id: "m" }, sessionManager: sm(id) })
const reply = (model) => ({ role: "assistant", provider: "fake", model, stopReason: "stop", timestamp: Date.now(), durationMs: 1, content: [{ type: "text", text: "SECRET" }] })
const read = async (id, call, path) => {
  await handlers.tool_execution_start({ toolCallId: call, toolName: "read", args: { path } }, ctx(id))
  await handlers.tool_call({ toolCallId: call, toolName: "read", input: { path } }, ctx(id))
  await handlers.tool_execution_end({ toolCallId: call, toolName: "read", result: { content: [{ type: "text", text: "SECRET" }] }, isError: false, durationMs: 1 }, ctx(id))
}

// 1. The reach check fails: the read and the reply that follows are not exported.
await handlers.session_start({ reason: "startup" }, ctx("s-fail"))
await read("s-fail", "c1", "/repoB/secret.go")
await handlers.turn_end({ messageEntryId: "e1", message: reply("m-tainted") }, ctx("s-fail"))

// 2. A new run of the session (a resume) starts clean.
await handlers.session_start({ reason: "resume" }, ctx("s-fail"))
await handlers.turn_end({ messageEntryId: "e2", message: reply("m-resumed") }, ctx("s-fail"))

// 3. A slow reach check: the read's span ends after it answered.
await handlers.session_start({ reason: "startup" }, ctx("s-slow"))
await read("s-slow", "c2", "/repoA/x.go")
await handlers.turn_end({ messageEntryId: "e3", message: reply("m-slow") }, ctx("s-slow"))
await handlers.session_shutdown({ reason: "quit" }, ctx("s-slow"))
await handlers.session_shutdown({ reason: "quit" }, ctx("s-fail"))
`
	script := filepath.Join(dir, "driver.mjs")
	if err := os.WriteFile(script, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, script, ext).CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	markerInfo, err := os.Stat(reachDoneMarker)
	if err != nil {
		t.Fatalf("slow reach marker not written: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	var sawResumed, sawSlowRead bool
	for _, sp := range spans {
		switch {
		case sp.Session == "s-fail" && sp.Name == "chat m-resumed":
			sawResumed = true
		case sp.Session == "s-fail":
			t.Errorf("failed pi-tool-reach leaked span %+v", sp)
		case sp.Session == "s-slow" && sp.Name == "execute_tool read":
			sawSlowRead = true
			if sp.EndNano < markerInfo.ModTime().UnixNano()-int64(50*time.Millisecond) {
				t.Errorf("read span end %d precedes its reach check %d", sp.EndNano, markerInfo.ModTime().UnixNano())
			}
		}
	}
	if !sawResumed || !sawSlowRead {
		t.Fatalf("resumed reply %v, slow read %v; spans=%+v", sawResumed, sawSlowRead, spans)
	}
}
