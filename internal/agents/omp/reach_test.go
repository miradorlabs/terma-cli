package omp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

func TestOmpStartRunAndToolReachCrossOrgMixed(t *testing.T) {
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

	// Subagent session-start does not call StartRun; parent session-start does.
	parentID := "018f3a2c-9101-7000-8000-000000000001"
	childID := "018f3a2c-9101-7000-8000-000000000002"
	qA := strconv.Quote(repoA)
	if err := events["omp-session-start"](ctx, env(`{"session_id":"`+parentID+`","cwd":`+qA+`}`)); err != nil {
		t.Fatal(err)
	}
	parentRun, ok := runs.Locate(stateDir, parentID)
	if !ok || parentRun.Run == "" {
		t.Fatalf("omp-session-start did not start run: %+v (ok=%v)", parentRun, ok)
	}
	if err := events["omp-session-start"](ctx, env(`{"session_id":"`+childID+`","cwd":`+qA+`,"parent_session_id":"`+parentID+`"}`)); err != nil {
		t.Fatal(err)
	}
	if p, ok := runs.Locate(stateDir, childID); !ok || p.Run != parentRun.Run {
		t.Fatalf("subagent omp-session-start run = %q (ok=%v); want parent run %q", p.Run, ok, parentRun.Run)
	}

	// Cross-org Read, Grep, Glob, and Bash each mark the run Mixed.
	reaches := []map[string]any{
		{"tool": "read", "paths": []string{fileB}},
		{"tool": "grep", "paths": []string{repoB}, "globs": []string{"*.go"}},
		{"tool": "glob", "globs": []string{"../beta/**/*.go"}},
		{"tool": "bash", "command": "cat ../beta/secret.go"},
	}
	for i, payload := range reaches {
		sid := "018f3a2c-9102-7000-8000-00000000000" + string(rune('1'+i))
		if err := events["omp-session-start"](ctx, env(`{"session_id":"`+sid+`","cwd":`+qA+`}`)); err != nil {
			t.Fatal(err)
		}
		payload["session_id"] = sid
		payload["cwd"] = repoA
		body, _ := json.Marshal(payload)
		if err := events["omp-tool-reach"](ctx, env(string(body))); err != nil {
			t.Fatal(err)
		}
		if c, ok := claim.Read(stateDir, sid, now); !ok || c.Kind != claim.KindMixed {
			t.Fatalf("%v: claim = %+v (ok=%v); want KindMixed", payload["tool"], c, ok)
		}
	}
}

func TestOmpRefreshMachineUpdatesInstalledExtension(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envAgentDir, dir)
	path := filepath.Join(dir, extensionPath)

	// Absent file: no-op.
	if got, changed, err := (Agent{}).RefreshMachine(); err != nil || changed || got != path {
		t.Fatalf("RefreshMachine(absent) = (%q, %v, %v); want (%q, false, nil)", got, changed, err, path)
	}

	// Inert raw template: no-op.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(extensionTemplate), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := (Agent{}).RefreshMachine(); err != nil || changed {
		t.Fatalf("RefreshMachine(inert) = (%v, %v); want (false, nil)", changed, err)
	}

	// Outdated installed extension: rewritten with current template, config preserved.
	old := `// Terma old extension
const CONFIG: TermaConfig | null = {"version":1,"hookCommand":["/custom/terma","hook"],"relay":"http://127.0.0.1:43180"} /* terma:config */
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
	for _, want := range []string{`"/custom/terma"`, `"http://127.0.0.1:43180"`, `"omp-tool-reach"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("refreshed extension missing %s", want)
		}
	}
	// Second refresh is a no-op.
	if _, changed, err := (Agent{}).RefreshMachine(); err != nil || changed {
		t.Fatalf("RefreshMachine(current) = (%v, %v); want (false, nil)", changed, err)
	}
}

func TestOmpToolReachAwaitsFenceAndBlocksOnDoubleFailureUnderNode(t *testing.T) {
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

	// Subtest 1: first reach fails, fence sleeps 250ms then writes marker and exits 0.
	// tool_call must NOT return until the fence marker is already on disk, and must not block.
	t.Run("awaits_fence_before_returning", func(t *testing.T) {
		dir := t.TempDir()
		fencedFile := filepath.Join(dir, "fenced")
		editFile := filepath.Join(dir, "edit.called")
		hook := filepath.Join(dir, "hook.sh")
		hookSrc := "#!/bin/sh\nbody=$(cat)\n" +
			"if [ \"$1\" = \"omp-tool-reach\" ]; then\n" +
			"  case \"$body\" in\n" +
			"    *TERMA_REACH_FAILED*) sleep 0.25; echo ok > " + strconv.Quote(fencedFile) + "; exit 0 ;;\n" +
			"    *) exit 1 ;;\n" +
			"  esac\n" +
			"fi\n" +
			"if [ \"$1\" = \"omp-file-edit\" ]; then echo called > " + strconv.Quote(editFile) + "; fi\n" +
			"exit 0\n"
		if err := os.WriteFile(hook, []byte(hookSrc), 0o700); err != nil {
			t.Fatal(err)
		}
		ext := filepath.Join(dir, "terma-ext.ts")
		if _, err := writeExtension(ext, []string{hook}, relayConfig().Endpoint); err != nil {
			t.Fatal(err)
		}
		script := `
import { existsSync } from "node:fs"
const [ext, marker] = process.argv.slice(2)
const h = {}
const mod = await import(ext)
await mod.default({ on: (n, fn) => { h[n] = fn } })
const ctx = { cwd: "/work", sessionManager: { getSessionId: () => "ses_fence", getSessionFile: () => "", getHeader: () => ({}) } }
await h.session_start({ reason: "startup" }, ctx)
const res = await h.tool_call({ toolName: "read", toolCallId: "c1", input: { path: "/other/secret.go" } }, ctx)
const fencedBeforeReturn = existsSync(marker)
await h.tool_result({ toolName: "write", toolCallId: "c2", isError: false, input: { path: "a.go" }, details: { resolvedPath: "/work/a.go" } }, ctx)
console.log(JSON.stringify({ res: res ?? null, fencedBeforeReturn }))
`
		driverPath := filepath.Join(dir, "driver.mjs")
		if err := os.WriteFile(driverPath, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(node, driverPath, ext, fencedFile).CombinedOutput()
		if err != nil {
			t.Fatalf("node: %v\n%s", err, out)
		}
		var got struct {
			Res                map[string]any `json:"res"`
			FencedBeforeReturn bool           `json:"fencedBeforeReturn"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("parse output %q: %v", out, err)
		}
		if !got.FencedBeforeReturn {
			t.Fatalf("tool_call returned before unknown-reach fence completed: %+v", got)
		}
		if got.Res != nil {
			t.Fatalf("tool_call unexpectedly blocked after fence succeeded: %+v", got.Res)
		}
		if _, err := os.Stat(editFile); !os.IsNotExist(err) {
			t.Fatalf("tainted session still emitted omp-file-edit")
		}
	})

	// Subtest 2: both reach and fence fail -> tool_call must fail closed with { block: true }.
	t.Run("blocks_tool_call_when_fence_fails", func(t *testing.T) {
		dir := t.TempDir()
		hook := filepath.Join(dir, "hook.sh")
		hookSrc := "#!/bin/sh\ncat > /dev/null\nif [ \"$1\" = \"omp-tool-reach\" ]; then exit 1; fi\nexit 0\n"
		if err := os.WriteFile(hook, []byte(hookSrc), 0o700); err != nil {
			t.Fatal(err)
		}
		ext := filepath.Join(dir, "terma-ext.ts")
		if _, err := writeExtension(ext, []string{hook}, relayConfig().Endpoint); err != nil {
			t.Fatal(err)
		}
		script := `
const [ext] = process.argv.slice(2)
const h = {}
const mod = await import(ext)
await mod.default({ on: (n, fn) => { h[n] = fn } })
const ctx = { cwd: "/work", sessionManager: { getSessionId: () => "ses_block", getSessionFile: () => "", getHeader: () => ({}) } }
await h.session_start({ reason: "startup" }, ctx)
const first = await h.tool_call({ toolName: "read", toolCallId: "c1", input: { path: "/other/secret.go" } }, ctx)
const second = await h.tool_call({ toolName: "bash", toolCallId: "c2", input: { command: "cat /other/secret.go" } }, ctx)
console.log(JSON.stringify({ first, second }))
`
		driverPath := filepath.Join(dir, "driver.mjs")
		if err := os.WriteFile(driverPath, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(node, driverPath, ext).CombinedOutput()
		if err != nil {
			t.Fatalf("node: %v\n%s", err, out)
		}
		var got struct {
			First struct {
				Block bool `json:"block"`
			} `json:"first"`
			Second struct {
				Block bool `json:"block"`
			} `json:"second"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("parse output %q: %v", out, err)
		}
		if !got.First.Block || !got.Second.Block {
			t.Fatalf("expected tool_call to fail closed with block=true when fence fails; got %s", out)
		}
	})

	// Subtest 3: after a successful fence, resuming the same session (new session_start)
	// must reset fence state so subsequent tool_call runs omp-tool-reach again.
	t.Run("resets_fence_on_session_resume", func(t *testing.T) {
		dir := t.TempDir()
		logFile := filepath.Join(dir, "reach.log")
		failAll := filepath.Join(dir, "fail_all")
		hook := filepath.Join(dir, "hook.sh")
		hookSrc := "#!/bin/sh\nbody=$(cat)\n" +
			"if [ \"$1\" = \"omp-tool-reach\" ]; then\n" +
			"  echo \"$body\" >> " + strconv.Quote(logFile) + "\n" +
			"  if [ -e " + strconv.Quote(failAll) + " ]; then exit 1; fi\n" +
			"  case \"$body\" in\n" +
			"    *TERMA_REACH_FAILED*) exit 0 ;;\n" +
			"    *) exit 1 ;;\n" +
			"  esac\n" +
			"fi\n" +
			"exit 0\n"
		if err := os.WriteFile(hook, []byte(hookSrc), 0o700); err != nil {
			t.Fatal(err)
		}
		ext := filepath.Join(dir, "terma-ext.ts")
		if _, err := writeExtension(ext, []string{hook}, relayConfig().Endpoint); err != nil {
			t.Fatal(err)
		}
		script := `
import { writeFileSync } from "node:fs"
const [ext, failAll] = process.argv.slice(2)
const h = {}
const mod = await import(ext)
await mod.default({ on: (n, fn) => { h[n] = fn } })
const ctx = { cwd: "/work", sessionManager: { getSessionId: () => "ses_resume", getSessionFile: () => "", getHeader: () => ({}) } }
await h.session_start({ reason: "startup" }, ctx)
const first = await h.tool_call({ toolName: "read", toolCallId: "c1", input: { path: "/other/a.go" } }, ctx)
// Resume the same session id: StartRun clears persistent Mixed, so fence must reset too.
writeFileSync(failAll, "1")
await h.session_start({ reason: "resume" }, ctx)
const afterResume = await h.tool_call({ toolName: "read", toolCallId: "c2", input: { path: "/other/b.go" } }, ctx)
console.log(JSON.stringify({ first: first ?? null, afterResume: afterResume ?? null }))
`
		driverPath := filepath.Join(dir, "driver.mjs")
		if err := os.WriteFile(driverPath, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(node, driverPath, ext, failAll).CombinedOutput()
		if err != nil {
			t.Fatalf("node: %v\n%s", err, out)
		}
		var got struct {
			First       map[string]any `json:"first"`
			AfterResume struct {
				Block bool `json:"block"`
			} `json:"afterResume"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("parse output %q: %v", out, err)
		}
		if got.First != nil {
			t.Fatalf("first call unexpectedly blocked: %+v", got.First)
		}
		if !got.AfterResume.Block {
			t.Fatalf("resumed session reused stale fence instead of running reach & failing closed: %s", out)
		}
		logBytes, _ := os.ReadFile(logFile)
		if !strings.Contains(string(logBytes), "/other/b.go") {
			t.Fatalf("resumed session did not invoke omp-tool-reach for /other/b.go; log:\n%s", logBytes)
		}
	})

	// Subtest 4: parent resumes while child's fence is in flight -> child fence completion
	// must NOT re-fence the resumed parent run.
	t.Run("child_fence_cannot_refence_resumed_parent", func(t *testing.T) {
		dir := t.TempDir()
		logFile := filepath.Join(dir, "reach.log")
		hook := filepath.Join(dir, "hook.sh")
		hookSrc := "#!/bin/sh\nbody=$(cat)\n" +
			"if [ \"$1\" = \"omp-tool-reach\" ]; then\n" +
			"  echo \"$body\" >> " + strconv.Quote(logFile) + "\n" +
			"  case \"$body\" in\n" +
			"    *TERMA_REACH_FAILED*) sleep 0.2; exit 0 ;;\n" +
			"    *child_fail*) exit 1 ;;\n" +
			"    *) exit 0 ;;\n" +
			"  esac\n" +
			"fi\n" +
			"exit 0\n"
		if err := os.WriteFile(hook, []byte(hookSrc), 0o700); err != nil {
			t.Fatal(err)
		}
		ext := filepath.Join(dir, "terma-ext.ts")
		if _, err := writeExtension(ext, []string{hook}, relayConfig().Endpoint); err != nil {
			t.Fatal(err)
		}
		script := `
const [ext] = process.argv.slice(2)
const h = {}
const mod = await import(ext)
await mod.default({ on: (n, fn) => { h[n] = fn } })
const dir = "/home/dev/.omp/agent/sessions/-work"
const pFile = dir + "/2026-11-21T13-14-22-769Z_parent-1.jsonl"
const sm = (id, file, header) => ({ getSessionId: () => id, getSessionFile: () => file, getHeader: () => header })
const parent = { cwd: "/work", sessionManager: sm("parent-1", pFile, { id: "parent-1" }) }
const child = { cwd: "/work", sessionManager: sm("child-1", pFile.replace(/\.jsonl$/, "") + "/Sub.jsonl", { parentSession: pFile }) }
await h.session_start({ reason: "startup" }, parent)
await h.session_start({ reason: "startup" }, child)
const childCall = h.tool_call({ toolName: "read", toolCallId: "c1", input: { path: "/other/child_fail.go" } }, child)
// Parent resumes while child's fence is still running.
const parentResume = h.session_start({ reason: "resume" }, parent)
await Promise.all([childCall, parentResume])
await h.tool_call({ toolName: "read", toolCallId: "p1", input: { path: "/other/parent_after_resume.go" } }, parent)
`
		driverPath := filepath.Join(dir, "driver.mjs")
		if err := os.WriteFile(driverPath, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(node, driverPath, ext).CombinedOutput(); err != nil {
			t.Fatalf("node: %v\n%s", err, out)
		}
		logBytes, _ := os.ReadFile(logFile)
		if !strings.Contains(string(logBytes), "parent_after_resume.go") {
			t.Fatalf("resumed parent skipped omp-tool-reach after concurrent child fence; log:\n%s", logBytes)
		}
	})

	// Subtest 5: resuming a tainted session when omp-session-start fails must keep the session
	// tainted (no prompts, tool_call fails closed) until a subsequent session_start succeeds.
	t.Run("keeps_taint_when_resumed_session_start_fails", func(t *testing.T) {
		dir := t.TempDir()
		logFile := filepath.Join(dir, "events.log")
		failStart := filepath.Join(dir, "fail_start")
		failReach := filepath.Join(dir, "fail_reach")
		hook := filepath.Join(dir, "hook.sh")
		hookSrc := "#!/bin/sh\nbody=$(cat)\n" +
			"echo \"$1:$body\" >> " + strconv.Quote(logFile) + "\n" +
			"if [ \"$1\" = \"omp-session-start\" ] && [ -e " + strconv.Quote(failStart) + " ]; then exit 1; fi\n" +
			"if [ \"$1\" = \"omp-tool-reach\" ] && [ -e " + strconv.Quote(failReach) + " ]; then exit 1; fi\n" +
			"exit 0\n"
		if err := os.WriteFile(hook, []byte(hookSrc), 0o700); err != nil {
			t.Fatal(err)
		}
		ext := filepath.Join(dir, "terma-ext.ts")
		if _, err := writeExtension(ext, []string{hook}, relayConfig().Endpoint); err != nil {
			t.Fatal(err)
		}
		script := `
import { unlinkSync, writeFileSync } from "node:fs"
const [ext, failStart, failReach] = process.argv.slice(2)
const h = {}
const mod = await import(ext)
await mod.default({ on: (n, fn) => { h[n] = fn } })
const ctx = { cwd: "/work", sessionManager: { getSessionId: () => "ses_taint", getSessionFile: () => "", getHeader: () => ({}) } }
await h.session_start({ reason: "startup" }, ctx)
writeFileSync(failReach, "1")
const taintedCall = await h.tool_call({ toolName: "read", toolCallId: "c1", input: { path: "/other/a.go" } }, ctx)
// Resume while omp-session-start fails: must remain tainted and block even when normal reach would pass.
writeFileSync(failStart, "1")
await h.session_start({ reason: "resume" }, ctx)
h.before_agent_start({}, ctx)
const blockedAfterFailedStart = await h.tool_call({ toolName: "read", toolCallId: "c2", input: { path: "/other/b.go" } }, ctx)
// Now let omp-session-start and reach succeed: taint must clear and prompt/reach resume.
unlinkSync(failStart)
unlinkSync(failReach)
await h.session_start({ reason: "resume" }, ctx)
h.before_agent_start({}, ctx)
const okAfterGoodStart = await h.tool_call({ toolName: "read", toolCallId: "c3", input: { path: "/other/c.go" } }, ctx)
console.log(JSON.stringify({ taintedCall, blockedAfterFailedStart, okAfterGoodStart: okAfterGoodStart ?? null }))
`
		driverPath := filepath.Join(dir, "driver.mjs")
		if err := os.WriteFile(driverPath, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(node, driverPath, ext, failStart, failReach).CombinedOutput()
		if err != nil {
			t.Fatalf("node: %v\n%s", err, out)
		}
		var got struct {
			TaintedCall             struct{ Block bool } `json:"taintedCall"`
			BlockedAfterFailedStart struct{ Block bool } `json:"blockedAfterFailedStart"`
			OkAfterGoodStart        map[string]any       `json:"okAfterGoodStart"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("parse output %q: %v", out, err)
		}
		if !got.TaintedCall.Block || !got.BlockedAfterFailedStart.Block {
			t.Fatalf("expected failed start to keep session tainted and block tool_call; got %s", out)
		}
		if got.OkAfterGoodStart != nil {
			t.Fatalf("expected successful resumed start to clear taint; got %s", out)
		}
		logBytes, _ := os.ReadFile(logFile)
		logStr := string(logBytes)
		if strings.Count(logStr, "omp-prompt:") != 1 {
			t.Fatalf("expected exactly 1 omp-prompt (only after successful resume); log:\n%s", logStr)
		}
		if !strings.Contains(logStr, "/other/c.go") {
			t.Fatalf("expected /other/c.go reach after successful resume; log:\n%s", logStr)
		}
	})
}
