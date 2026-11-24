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
	"runtime"
	"slices"
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
			if err := s.Bind(spec[2], spec[0], false); err != nil {
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
	for _, want := range []string{`"/custom/terma"`, `"http://127.0.0.1:43180"`, `"-tool-reach"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("refreshed extension missing %s", want)
		}
	}
	if _, changed, err := (Agent{}).RefreshMachine(); err != nil || changed {
		t.Fatalf("RefreshMachine(current) = (%v, %v); want (false, nil)", changed, err)
	}
}

func TestPiToolReachFailsClosedAndResultFallbackAwaitsUnderNode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell hook test")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	if out, err := exec.Command(node, "-p", "process.features.typescript").Output(); err != nil || !slices.Contains([]string{"strip", "transform"}, strings.TrimSpace(string(out))) {
		t.Skip("node does not run TypeScript")
	}
	dir := t.TempDir()
	reachDoneMarker := filepath.Join(dir, "fallback.done")

	type seenSpan struct {
		Name    string
		EndNano int64
		Session string
	}
	var mu sync.Mutex
	var spans []seenSpan
	var sawMarkerBeforeFallbackSpan bool
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
				_, markerErr := os.Stat(reachDoneMarker)
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
							if sid == "s-fallback" && sp.Name == "execute_tool read" && markerErr == nil {
								sawMarkerBeforeFallbackSpan = true
							}
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
		"    *'\"session_id\":\"s-fail\"'*) exit 1 ;;\n" +
		"    *'\"session_id\":\"s-fallback\"'*) sleep 0.15; touch " + strconv.Quote(reachDoneMarker) + " ;;\n" +
		"  esac\n" +
		"fi\n"
	if err := os.WriteFile(hook, []byte(hookSrc), 0o700); err != nil {
		t.Fatal(err)
	}
	ext := filepath.Join(dir, "terma.ts")
	if _, err := writeExtension(ext, relayExtension(agents.RelayConfig{Endpoint: srv.URL, Token: "relay-token", HookCommand: []string{hook}})); err != nil {
		t.Fatal(err)
	}
	driver := `
import { pathToFileURL } from "node:url"
const mod = await import(pathToFileURL(process.argv[2]).href)
const handlers = {}
mod.default({ on: (name, fn) => { handlers[name] = fn } })
const mkCtx = (id) => ({ cwd: "/repoA", sessionManager: { getSessionId: () => id } })

// 1. Reach exits 1 -> execute_tool AND follow-up chat span for s-fail must be suppressed.
await handlers.session_start({ reason: "startup" }, mkCtx("s-fail"))
await handlers.tool_call({ toolCallId: "c1", toolName: "read", input: { path: "/repoB/secret.go" } }, mkCtx("s-fail"))
await handlers.tool_result({ toolCallId: "c1", toolName: "read", input: { path: "/repoB/secret.go" }, content: "SECRET" }, mkCtx("s-fail"))
handlers.message_end({ message: { role: "assistant", model: "m", content: [{ type: "text", text: "SECRET" }] } }, mkCtx("s-fail"))

// 2. tool_result-only fallback (no tool_call) awaits reach BEFORE assigning end timestamp.
await handlers.session_start({ reason: "startup" }, mkCtx("s-fallback"))
await handlers.tool_result({ toolCallId: "c2", toolName: "read", input: { path: "/repoB/secret.go" }, content: "ok" }, mkCtx("s-fallback"))

await handlers.session_shutdown({}, mkCtx("s-fallback"))
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
		t.Fatalf("fallback reach marker not written: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !sawMarkerBeforeFallbackSpan {
		t.Fatalf("expected execute_tool read span for s-fallback only after reach finished; spans=%+v", spans)
	}
	for _, sp := range spans {
		if sp.Session == "s-fail" {
			t.Fatalf("failed pi-tool-reach leaked span %+v for tainted session s-fail", sp)
		}
		if sp.Session == "s-fallback" && sp.Name == "execute_tool read" {
			if sp.EndNano < markerInfo.ModTime().UnixNano()-int64(50*time.Millisecond) {
				t.Fatalf("fallback span end %d precedes reach completion %d", sp.EndNano, markerInfo.ModTime().UnixNano())
			}
		}
	}
}
