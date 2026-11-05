package e2e

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The files profile scenarios (Test…ProfileFiles): the profile's first turn, behind the
// recording proxy as the published turn is, with a tool turn after it (a shell command and a
// file written), traced (filetrace.go): what the build reads, writes and looks for at startup,
// during its turn and as it exits, the programs it starts and the sockets it uses, and whether
// it touches the decoy credentials planted in its home and repository (decoys.go). Each writes
// a profile row of its own, the probe "filesystem", which refgen publishes in the snapshot's
// filesystem section by platform. Linux traces with strace; macOS with eslogger, where it can
// run as root with Full Disk Access (TERMA_E2E_ESLOGGER_VIA); anywhere else the scenario is
// skipped, and says why.
//
// The run is a clean room (CleanRoom): no terma in it, no setup, hooks, relay or exporter of
// terma's, SHELL the OS's default login shell and HOME the sandbox's, so nothing of the
// machine's own (terma, the developer's shell startup files) is traced as the build's. The
// hooks probe's recorder (hooks_probe.go) is registered for every hook event the build knows,
// so the build runs as it does with hooks configured; the trace drops the recorder ("hooks"),
// and the snapshot's hooks section says what the build hands a hook.

// filesTraceFile names the scenario's own file edit.
const filesTraceFile = "notes.txt"

// traceRun is how this machine traces a run, or skips t with why it cannot.
func traceRun(t *testing.T) (string, func(cmd *exec.Cmd) ([]byte, []TraceEvent, int, error)) {
	t.Helper()
	switch runtime.GOOS {
	case "linux":
		if why := straceAvailable(); why != "" {
			t.Skip("files profile: " + why)
		}
		return "strace", func(cmd *exec.Cmd) ([]byte, []TraceEvent, int, error) { return runStrace(t, cmd, egressSettle) }
	case "darwin":
		l, why := esloggerAvailable()
		if l == nil {
			t.Skip("files profile: " + why)
		}
		return "eslogger", func(cmd *exec.Cmd) ([]byte, []TraceEvent, int, error) {
			return runESLogger(t, l, cmd, egressSettle)
		}
	}
	t.Skip("files profile: traced on Linux (strace) and macOS (eslogger) only")
	return "", nil
}

// TestFileTracerCheck traces a small shell script on this machine, with no harness: the
// tracer's own check, that it sees a process tree's files, programs and sockets and, on macOS,
// that eslogger leaves no root process behind.
func TestFileTracerCheck(t *testing.T) {
	if !Enabled() {
		t.Skip("live tests run only with TERMA_E2E=1")
	}
	tracer, run := traceRun(t)
	dir := t.TempDir()
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	script := "cat /etc/hosts >/dev/null; ls " + dir + "/absent 2>/dev/null; echo x > " + dir + "/made; mv " + dir + "/made " + dir + "/moved; git --version >/dev/null"
	cmd := exec.Command("/bin/sh", "-c", script)
	out, events, root, err := run(cmd)
	if err != nil {
		t.Fatalf("%s: %v\n%s", tracer, err, out)
	}
	probe := FilesOf(events, TraceScope{Platform: runtime.GOOS + "/" + runtime.GOARCH, Tracer: tracer, Root: root,
		Replace: map[string]string{dir: "<dir>"}})
	for path, op := range map[string]string{"<dir>/made": "rename", "<dir>/moved": "write"} {
		if u := probe.Paths[path]; u == nil || !slicesContains(u.Ops, op) {
			t.Errorf("%s: %s = %+v", tracer, path, u)
		}
	}
	if probe.Exec["git --version"] == nil {
		t.Errorf("%s saw no git --version among %v", tracer, mapsKeysOf(probe.Exec))
	}
	if runtime.GOOS == "darwin" {
		if left := esloggerLeftovers(); left != "" {
			t.Errorf("eslogger still running after the trace: %s", left)
		}
	}
	t.Logf("%s: %d events, %d paths, %d commands", tracer, len(events), len(probe.Paths), len(probe.Exec))
}

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func mapsKeysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// modelWindow is the run's model calls: from the first request to the last response.
func (p *EgressProxy) modelWindow() (time.Time, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var first, last time.Time
	for _, c := range p.calls {
		if first.IsZero() || c[0].Before(first) {
			first = c[0]
		}
		if c[1].After(last) {
			last = c[1]
		}
	}
	return first, last
}

// filesScope is the trace scope of b's run in sb: its paths' placeholders, the build's own
// installation, the hook recorder in rec, the tool calls' commands, the run's endpoints and the
// decoys.
func filesScope(b Binary, sb *Sandbox, tracer string, env []string, decoys *Decoys, proxy *EgressProxy, provider, rec string, tools []string) TraceScope {
	s := TraceScope{Platform: runtime.GOOS + "/" + runtime.GOARCH, Tracer: tracer, Replace: map[string]string{}, Endpoints: map[string]string{},
		Tools: tools}
	real := b.Path
	if r, err := filepath.EvalSymlinks(b.Path); err == nil {
		real = r
	}
	install := filepath.Dir(real)
	tmp := strings.TrimSuffix(os.TempDir(), "/")
	places := map[string]string{sb.Repo: "<repo>", sb.Home: "<home>", sb.Dir: "<sandbox>",
		sb.ClaudeConfig: "<claude-config>", sb.CodexHome: "<codex-home>", install: "<install>", tmp: "<tmp>",
		filepath.Dir(sb.Dir): "<sandbox>/..", "/tmp": "<tmp>", filepath.Dir(install): "<install>/.."}
	// The temporary folder's own folders (macOS's per-user /var/folders/xx/…/T), and the home of
	// the user the test runs as, which a program reads through the user database rather than
	// HOME: neither is the run's to publish.
	if up := filepath.Dir(tmp); len(up) > 5 {
		places[up] = "<tmp>/.."
		if upup := filepath.Dir(up); len(upup) > 5 && upup != "/var/folders" {
			places[upup] = "<tmp>/../.."
		}
	}
	if home, err := os.UserHomeDir(); err == nil && len(home) > 5 && home != sb.Home {
		places[home] = "<user-home>"
	}
	for lit, name := range places {
		for _, form := range bothForms(lit) {
			if _, set := s.Replace[form]; !set || len(name) > len(s.Replace[form]) {
				s.Replace[form] = name
			}
		}
	}
	s.Replace["/tmp"], s.Replace["/private/tmp"] = "<tmp>", "<tmp>"
	if tmp != "/tmp" {
		s.Replace[tmp] = "<tmp>"
	}
	for _, p := range []string{b.Path, real, install, filepath.Join(sb.Dir, "path", b.Harness)} {
		s.Self = append(s.Self, bothForms(p)...)
	}
	// The recorder: the test binary, started as the hook, and the folder it saves each run in.
	hooks := []string{rec}
	if exe, err := os.Executable(); err == nil {
		hooks = append(hooks, exe)
		if r, err := filepath.EvalSymlinks(exe); err == nil && r != exe {
			hooks = append(hooks, r)
		}
	}
	for _, p := range hooks {
		s.Hooks = append(s.Hooks, bothForms(p)...)
	}
	s.Test = append(s.Test, bothForms(filepath.Dir(proxy.CAFile))...)
	// The checkout the suite runs from (test/e2e's repository): the builds it downloads, the
	// logs a backfill writes, which the build inherits as its output, are in it.
	// The machine image mounts test/e2e alone (/work/e2e): there it is the checkout.
	if wd, err := os.Getwd(); err == nil {
		checkout := wd
		if filepath.Base(wd) == "e2e" && filepath.Base(filepath.Dir(wd)) == "test" {
			checkout = filepath.Dir(filepath.Dir(wd))
		}
		if strings.Count(checkout, "/") < 2 {
			checkout = wd // never a folder as wide as /work: the machine's own files are in it
		}
		s.Test = append(s.Test, bothForms(checkout)...)
		if home, err := os.UserHomeDir(); err == nil && len(home) > 1 {
			for d := filepath.Dir(checkout); strings.HasPrefix(d, home+"/"); d = filepath.Dir(d) {
				s.TestWay = append(s.TestWay, bothForms(d)...)
			}
		}
	}
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			s.Path = v
			for _, d := range strings.Split(v, ":") {
				if d != "" {
					s.PathDirs = append(s.PathDirs, bothForms(filepath.Clean(d))...)
				}
			}
		}
	}
	for addr, name := range map[string]string{urlHost(provider): "<provider>", urlHost(proxy.URL): "<proxy>"} {
		if addr != "" {
			s.Endpoints[addr] = name
			if _, port, ok := strings.Cut(addr, ":"); ok {
				s.Endpoints["localhost:"+port] = name
			}
		}
	}
	s.TurnStart, s.TurnEnd = proxy.modelWindow()
	decoys.Scope(&s)
	for _, d := range append([]string(nil), s.Decoys...) {
		s.Decoys = append(s.Decoys, bothForms(d)...)
	}
	return s
}

// bothForms is a path as macOS spells it both ways (/var and /private/var, /tmp and
// /private/tmp), or the path alone elsewhere.
func bothForms(path string) []string {
	for _, top := range []string{"/var/", "/tmp/", "/etc/"} {
		if strings.HasPrefix(path, top) {
			return []string{path, "/private" + path}
		}
		if strings.HasPrefix(path, "/private"+top) {
			return []string{path, strings.TrimPrefix(path, "/private")}
		}
	}
	return []string{path}
}

// nativeBuild is b as the build's own executable: an npm installation's JavaScript launcher
// (Codex's codex.js) resolved to the native binary it starts, as a downloaded build is run, so
// a trace of either starts with the build and not with Node.
func nativeBuild(b Binary) Binary {
	real, err := filepath.EvalSymlinks(b.Path)
	if err != nil || !strings.HasSuffix(real, ".js") {
		return b
	}
	pkg := filepath.Dir(filepath.Dir(real))
	for _, pattern := range []string{"node_modules/@openai/codex-*/vendor/*/bin/codex", "vendor/*/codex/codex", "vendor/*/bin/codex"} {
		if m, _ := filepath.Glob(filepath.Join(pkg, pattern)); len(m) > 0 {
			b.Path = m[0]
			return b
		}
	}
	return b
}

// urlHost is a URL's host:port.
func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// recordFiles records the files profile of a traced run in t's profile of b.
func recordFiles(t *testing.T, b Binary, events []TraceEvent, root int, scope TraceScope, decoys *Decoys) *FilesProbe {
	t.Helper()
	scope.Root = root
	probe := FilesOf(events, scope)
	decoys.Connected(probe, scope)
	RecordProbe(t, b, "filesystem", probe)
	t.Logf("files profile: %d paths, %d commands, %d decoys touched, dropped %v", len(probe.Paths), len(probe.Exec), touchedDecoys(probe), probe.Dropped)
	return probe
}

// filesHookRecorder registers the hooks probe's recorder in rec for every hook event b knows
// (HookEventsOf), in the harness's own user settings in sb, as a developer's hooks are: Claude
// Code's settings.json, Codex's hooks.json. One entry an event, whose matcher matches everything.
func filesHookRecorder(t *testing.T, b Binary, sb *Sandbox, rec string) {
	t.Helper()
	events, source, err := HookEventsOf(b)
	if err != nil {
		t.Logf("hook events of %s %s: %v", b.Harness, b.Version, err)
	}
	t.Logf("%s %s: a recording hook for each of %d hook events (%s)", b.Harness, b.Version, len(events), source)
	hooks := map[string]any{}
	for _, e := range events {
		cmd, err := HookRecorderCommand(rec, e, hookMatchAll)
		if err != nil {
			t.Fatal(err)
		}
		group := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": cmd, "timeout": 30}}}
		if b.Harness == "claude" {
			// FileChanged's matcher names the files the build watches: the repository's README.
			group["matcher"] = map[bool]string{true: "README.md", false: "*"}[e == "FileChanged"]
		}
		hooks[e] = []any{group}
	}
	switch b.Harness {
	case "claude":
		editJSON(t, filepath.Join(sb.ClaudeConfig, "settings.json"), func(doc map[string]any) { doc["hooks"] = hooks })
	case "codex":
		data, _ := json.MarshalIndent(map[string]any{"hooks": hooks}, "", "  ")
		sb.writeAbs(filepath.Join(sb.CodexHome, "hooks.json"), string(data)+"\n")
	}
}

// checkHookRecorder fails t where the build knows hook events and the recorder saw none run:
// the build did not run as it does with hooks configured. trust is what the scenario did about
// the build's hook trust (Codex's), which the failure names where the hooks are not trusted.
func checkHookRecorder(t *testing.T, b Binary, rec string, trust CodexHookTrust) {
	t.Helper()
	events, _, _ := HookEventsOf(b)
	calls := HookCallsIn(rec)
	fired := map[string]int{}
	for _, c := range calls {
		fired[c.Event]++
	}
	t.Logf("hooks run: %v", fired)
	if len(events) > 0 && len(calls) == 0 {
		why := ""
		if trust.Gated && !trust.Trusted() {
			why = ", its " + trust.String()
		}
		t.Errorf("%s %s knows %d hook events and ran none of the recorder's%s", b.Harness, b.Version, len(events), why)
	}
}

func touchedDecoys(p *FilesProbe) int {
	n := 0
	for _, level := range p.Decoys {
		if level != decoyLevels[0] {
			n++
		}
	}
	return n
}

// TestClaudeProfileFiles traces each Claude Code build's profile turn, with a tool turn: a
// shell command and a file written.
func TestClaudeProfileFiles(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		tracer, run := traceRun(t)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b), CleanRoom())
		rec := filepath.Join(sb.Dir, "hook-recorder")
		filesHookRecorder(t, b, sb, rec)
		decoys := PlantDecoys(t, sb)
		proxy := StartEgressProxy(t)
		var calls atomic.Int32
		tools := []string{"git status --short && git log --oneline -1"}
		steps := []claudeStep{
			{bash(tools[0])},
			{{"name": "Write", "input": map[string]any{"file_path": filepath.Join(sb.Repo, filesTraceFile), "content": "written by the files profile\n"}}},
		}
		provider := httptest.NewServer(proxy.Provider(isClaudeModelCall, CaptureProbe(t, b, "filesystem", claudeWorkloadProvider(&calls, steps, false))))
		defer provider.Close()
		sb.ClaudeBaseURL = provider.URL
		PlaceSandbox(t, b, sb)
		Placeholder(t, b, provider.URL, "<provider>")
		Placeholder(t, b, profilePrompt, "<prompt>")
		args := []string{"--output-format", "json", "--max-turns", "4", "--session-id", uuid.NewString(), "--allowedTools", "Bash,Write"}
		RecordCommand(t, b, args)
		ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, sb.claudeLauncher(), append([]string{"-p", profilePrompt}, args...)...)
		cmd.Dir = sb.workDir()
		cmd.Env = append(append(sb.claudeEnv(RouteAPIKey), proxy.Env()...), decoys.Env...)
		out, events, root, err := run(cmd)
		scope := filesScope(b, sb, tracer, cmd.Env, decoys, proxy, provider.URL, rec, tools)
		recordFiles(t, b, events, root, scope, decoys)
		if err != nil {
			t.Fatalf("claude -p, traced: %v\n%s", err, out)
		}
		checkHookRecorder(t, b, rec, CodexHookTrust{})
		if calls.Load() == 0 {
			t.Fatal("Claude Code sent its provider no message request")
		}
		if _, err := os.Stat(filepath.Join(sb.Repo, filesTraceFile)); err != nil {
			t.Errorf("the tool turn wrote no %s: %v\n%s", filesTraceFile, err, out)
		}
	})
}

// TestCodexProfileFiles is the same for each Codex build's `codex exec`: its shell runs the
// command, then writes the file, in the workspace its default sandbox lets it write.
func TestCodexProfileFiles(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		tracer, run := traceRun(t)
		t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithCodex(b), CleanRoom())
		sb.prepareCodex(RouteAPIKey)
		rec := filepath.Join(sb.Dir, "hook-recorder")
		filesHookRecorder(t, b, sb, rec)
		decoys := PlantDecoys(t, sb)
		proxy := StartEgressProxy(t)
		var calls atomic.Int32
		cmds := []string{"git status --short && git log --oneline -1", "printf 'written by the files profile\\n' > " + filesTraceFile}
		provider := httptest.NewServer(proxy.Provider(isCodexModelCall, CaptureProbe(t, b, "filesystem", codexWorkloadProvider(&calls, cmds))))
		defer provider.Close()
		PlaceSandbox(t, b, sb)
		Placeholder(t, b, provider.URL, "<provider>")
		Placeholder(t, b, profilePrompt, "<prompt>")
		// A build that keeps hooks behind a feature has it turned on, as a developer with hooks does.
		args := append(append(codexProfileArgs(b, sb, provider.URL), codexHookConfig(sb)...), "--sandbox", "workspace-write")
		// A build with hook trust runs them once trusted, as the developer's review in Codex does.
		trust := trustCodexHooks(t, sb, args)
		t.Logf("%s %s: %s", b.Harness, b.Version, trust)
		RecordCommand(t, b, args)
		native := nativeBuild(b)
		ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, native.Path, append(args, profilePrompt)...)
		cmd.Dir = sb.workDir()
		cmd.Env = append(append(sb.codexEnv(RouteAPIKey), proxy.Env()...), decoys.Env...)
		out, events, root, err := run(cmd)
		scope := filesScope(native, sb, tracer, cmd.Env, decoys, proxy, provider.URL, rec, cmds)
		recordFiles(t, b, events, root, scope, decoys)
		if err != nil {
			t.Fatalf("codex exec, traced: %v\n%s", err, out)
		}
		checkHookRecorder(t, b, rec, trust)
		if calls.Load() == 0 {
			t.Fatal("Codex sent its provider no responses request")
		}
		if _, err := os.Stat(filepath.Join(sb.Repo, filesTraceFile)); err != nil {
			t.Errorf("the tool turn wrote no %s: %v\n%s", filesTraceFile, err, out)
		}
	})
}
