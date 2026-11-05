package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Codex Desktop, the IDE extension and — since 0.157, by default — the interactive TUI
// run their threads in `codex app-server`, one process serving every workspace: it
// spawns the hooks and exports all the telemetry. AppServer drives one over stdio
// JSON-RPC the way Desktop does: initialize with a client name, then thread/start (or
// thread/resume) with a cwd and turn/start, waiting for turn/completed. Verified against the 0.159 schema
// (`codex app-server generate-json-schema`).

// AppServer is a running `codex app-server` and its JSON-RPC client.
type AppServer struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr strings.Builder

	mu      sync.Mutex
	cond    *sync.Cond
	nextID  int
	results map[int]map[string]any
	notes   []map[string]any
	closed  bool
	once    sync.Once
}

// StartAppServer starts `codex app-server` in the sandbox against providerURL and
// introduces itself as client (Desktop names itself "Codex Desktop"; the first client
// sets the process's originator).
func (sb *Sandbox) StartAppServer(b Binary, client, providerURL string) *AppServer {
	t := sb.T
	t.Helper()
	sb.prepareCodex(RouteAPIKey)
	args := append([]string{"app-server", "-c", `cli_auth_credentials_store="file"`, "-c", "features.plugins=false",
		"-c", "features.remote_plugin=false", "-c", `model_reasoning_effort="low"`}, fixtureCodexArgs(providerURL)...)
	// fixtureCodexArgs ends with the CLI's sandbox flag, which app-server does not take:
	// each thread names its sandbox instead.
	args = args[:len(args)-2]
	a, err := launchAppServer(t, b.Path, args, sb.Repo, sb.codexEnv(RouteAPIKey), client)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// launchAppServer starts the app-server the Codex at path runs with args (app-server and its
// options), in dir with env, and introduces itself as client. It closes with t.
func launchAppServer(t *testing.T, path string, args []string, dir string, env []string, client string) (*AppServer, error) {
	t.Helper()
	a := &AppServer{t: t, results: map[int]map[string]any{}}
	a.cond = sync.NewCond(&a.mu)
	a.cmd = exec.Command(path, args...)
	a.cmd.Dir = dir
	a.cmd.Env = env
	a.cmd.Stderr = &a.stderr
	var err error
	if a.stdin, err = a.cmd.StdinPipe(); err != nil {
		return nil, err
	}
	stdout, err := a.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := a.cmd.Start(); err != nil {
		return nil, err
	}
	go a.read(stdout)
	t.Cleanup(a.Close)
	if _, err := a.tryCall("initialize", map[string]any{"clientInfo": map[string]any{"name": client, "title": client, "version": "1.0.0"},
		"capabilities": map[string]any{"experimentalApi": true}}); err != nil {
		a.Close()
		return nil, err
	}
	if err := a.send(map[string]any{"jsonrpc": "2.0", "method": "initialized"}); err != nil {
		a.Close()
		return nil, err
	}
	return a, nil
}

// PID is the app-server's process: every thread's exporter and hook parent.
func (a *AppServer) PID() int { return a.cmd.Process.Pid }

func (a *AppServer) send(m map[string]any) error {
	data, _ := json.Marshal(m)
	if _, err := a.stdin.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("app-server: %w\n%s", err, tail(a.stderr.String(), 2000))
	}
	return nil
}

func (a *AppServer) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		id, hasID := m["id"].(float64)
		method, _ := m["method"].(string)
		switch {
		case hasID && method != "":
			// A request of the server's (an approval): these threads never ask, and a
			// refusal is the safe answer if one does.
			_ = a.send(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": map[string]any{"decision": "decline"}})
		case hasID:
			a.mu.Lock()
			a.results[int(id)] = m
			a.cond.Broadcast()
			a.mu.Unlock()
		case method != "" && !strings.Contains(strings.ToLower(method), "delta"):
			a.mu.Lock()
			a.notes = append(a.notes, m)
			a.cond.Broadcast()
			a.mu.Unlock()
		}
	}
	a.mu.Lock()
	a.closed = true
	a.cond.Broadcast()
	a.mu.Unlock()
}

// wait blocks until ok holds (checked under the lock) or the timeout passes.
func (a *AppServer) wait(timeout time.Duration, ok func() bool) bool {
	deadline := time.Now().Add(timeout)
	timer := time.AfterFunc(timeout, func() {
		a.mu.Lock()
		a.cond.Broadcast()
		a.mu.Unlock()
	})
	defer timer.Stop()
	a.mu.Lock()
	defer a.mu.Unlock()
	for !ok() {
		if a.closed || time.Now().After(deadline) {
			return ok()
		}
		a.cond.Wait()
	}
	return true
}

func (a *AppServer) call(method string, params any) map[string]any {
	a.t.Helper()
	r, err := a.tryCall(method, params)
	if err != nil {
		a.t.Fatal(err)
	}
	return r
}

// tryCall is call, its failure returned.
func (a *AppServer) tryCall(method string, params any) (map[string]any, error) {
	a.mu.Lock()
	a.nextID++
	id := a.nextID
	a.mu.Unlock()
	if err := a.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	var res map[string]any
	if !a.wait(60*time.Second, func() bool { res = a.results[id]; return res != nil }) {
		return nil, fmt.Errorf("app-server %s: no response\n%s", method, tail(a.stderr.String(), 2000))
	}
	if e, ok := res["error"]; ok {
		return nil, fmt.Errorf("app-server %s: %v", method, e)
	}
	r, _ := res["result"].(map[string]any)
	return r, nil
}

// threadParams are what Desktop sends for a thread in cwd; trustHooks stands in for
// the developer's approval of the repository's hooks (Settings → Hooks), as
// --dangerously-bypass-hook-trust does for `codex exec`.
func threadParams(cwd string, trustHooks bool) map[string]any {
	p := map[string]any{"cwd": cwd, "approvalPolicy": "never", "sandbox": "workspace-write"}
	if trustHooks {
		p["config"] = map[string]any{"bypass_hook_trust": true}
	}
	return p
}

// ThreadStart starts a thread in cwd and returns its id — the conversation.id its
// telemetry carries.
func (a *AppServer) ThreadStart(cwd string, trustHooks bool) string {
	a.t.Helper()
	res := a.call("thread/start", threadParams(cwd, trustHooks))
	th, _ := res["thread"].(map[string]any)
	id, _ := th["id"].(string)
	if id == "" {
		a.t.Fatalf("thread/start returned no thread: %v", res)
	}
	return id
}

// ThreadResume resumes thread in cwd and returns the cwd the thread now runs in. A
// thread still loaded in the process keeps its own (Codex ignores the cwd), so
// resuming it somewhere else takes Unload first.
func (a *AppServer) ThreadResume(thread, cwd string, trustHooks bool) string {
	a.t.Helper()
	p := threadParams(cwd, trustHooks)
	p["threadId"] = thread
	res := a.call("thread/resume", p)
	got, _ := res["cwd"].(string)
	if th, ok := res["thread"].(map[string]any); ok && got == "" {
		got, _ = th["cwd"].(string)
	}
	return got
}

// Unload ends this client's subscription to thread and waits until the process no
// longer holds it loaded (thread/loaded/list), as when Desktop closes a thread.
func (a *AppServer) Unload(thread string) {
	a.t.Helper()
	a.call("thread/unsubscribe", map[string]any{"threadId": thread})
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		res := a.call("thread/loaded/list", map[string]any{})
		if !strings.Contains(fmt.Sprint(res), thread) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	a.t.Fatalf("thread %s stayed loaded after thread/unsubscribe", thread)
}

// Turn runs one turn of thread and waits for it to complete.
func (a *AppServer) Turn(thread, text string) {
	a.t.Helper()
	a.mu.Lock()
	from := len(a.notes)
	a.mu.Unlock()
	a.call("turn/start", map[string]any{"threadId": thread, "input": []any{map[string]any{"type": "text", "text": text}}})
	var status string
	done := a.wait(scenarioTimeout, func() bool {
		for _, n := range a.notes[from:] {
			if n["method"] != "turn/completed" {
				continue
			}
			p, _ := n["params"].(map[string]any)
			if p["threadId"] != thread {
				continue
			}
			turn, _ := p["turn"].(map[string]any)
			status = fmt.Sprint(turn["status"])
			return true
		}
		return false
	})
	if !done {
		a.t.Fatalf("thread %s: turn never completed\n%s", thread, tail(a.stderr.String(), 2000))
	}
	if status != "completed" {
		a.t.Errorf("thread %s: turn %s", thread, status)
	}
}

// Close ends the app-server: stdin closes, and it exits on its own.
func (a *AppServer) Close() { a.once.Do(a.close) }

func (a *AppServer) close() {
	if a.stdin != nil {
		_ = a.stdin.Close()
	}
	done := make(chan struct{})
	go func() { _ = a.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		_ = a.cmd.Process.Kill()
		<-done
	}
}

// TrustHooks records the developer's approval of every hook Codex finds for cwd, as
// Desktop's Settings → Hooks → Review (or the TUI's /hooks) does: the hooks/list key and
// current hash of each, as [hooks.state."<key>"] trusted_hash in config.toml. An entry
// `terma setup` already approved is left as it is, and must carry Codex's own hash. It
// returns how many hooks Codex listed.
func (a *AppServer) TrustHooks(sb *Sandbox, cwd string) int {
	a.t.Helper()
	res := a.call("hooks/list", map[string]any{"cwds": []string{cwd}})
	cfg := filepath.Join(sb.CodexHome, "config.toml")
	existing, _ := os.ReadFile(cfg)
	approved := trustedHashes(string(existing))
	hooks := listedHooks(res)
	var entries []string
	for _, h := range hooks {
		switch have, ok := approved[h.Key]; {
		case !ok:
			entries = append(entries, "[hooks.state."+tomlQuote(h.Key)+"]\ntrusted_hash = "+tomlQuote(h.Hash)+"\n")
		case have != h.Hash:
			a.t.Fatalf("terma approved %s as %s, but Codex hashes it %s", h.Key, have, h.Hash)
		}
	}
	if err := os.WriteFile(cfg, append(existing, []byte("\n"+strings.Join(entries, "\n"))...), 0o600); err != nil {
		a.t.Fatal(err)
	}
	return len(hooks)
}

// codexListedHook is one hook a hooks/list response lists: its key, Codex's hash of it now,
// and its trust status ("" where the build reports none).
type codexListedHook struct {
	Key, Hash, Status string
	Managed           bool
}

// listedHooks are the hooks a hooks/list response lists, wherever they sit in it: an entry
// with a key and a current hash (HookMetadata).
func listedHooks(res any) []codexListedHook {
	var out []codexListedHook
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			key, _ := x["key"].(string)
			hash, _ := x["currentHash"].(string)
			if key != "" && hash != "" {
				h := codexListedHook{Key: key, Hash: hash}
				h.Status, _ = x["trustStatus"].(string)
				h.Managed, _ = x["isManaged"].(bool)
				out = append(out, h)
			}
			for _, e := range x {
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(res)
	slices.SortFunc(out, func(a, b codexListedHook) int { return strings.Compare(a.Key, b.Key) })
	return out
}

// CodexHookTrust is what trustCodexHooks found and did. A build with hook trust (0.129 on)
// runs a hook of the developer's only once they have trusted its current hash; `codex exec`
// before 0.141 does not carry --dangerously-bypass-hook-trust into its threads, so there
// the hooks have to be trusted indeed.
type CodexHookTrust struct {
	// Gated says the build has hook trust: its hooks/list reports each hook's trustStatus.
	Gated bool
	// Statuses counts the hooks Codex lists by trust status, once the trust is written.
	Statuses map[string]int
	// Err is why the hooks could not be trusted.
	Err error
}

// Trusted reports whether every hook Codex lists runs: it is trusted or managed.
func (h CodexHookTrust) Trusted() bool {
	if h.Err != nil {
		return false
	}
	for s, n := range h.Statuses {
		if n > 0 && !strings.EqualFold(s, "trusted") && !strings.EqualFold(s, "managed") {
			return false
		}
	}
	return true
}

// Probe is the trust as the hooks probe publishes it: "" for a build without hook trust,
// "trusted" where every hook was trusted, else "untrusted".
func (h CodexHookTrust) Probe() string {
	switch {
	case !h.Gated:
		return ""
	case h.Trusted():
		return "trusted"
	}
	return "untrusted"
}

func (h CodexHookTrust) String() string {
	if !h.Gated {
		return "no hook trust in this build"
	}
	var parts []string
	for _, s := range slices.Sorted(maps.Keys(h.Statuses)) {
		parts = append(parts, fmt.Sprintf("%d %s", h.Statuses[s], s))
	}
	out := "hook trust: " + strings.Join(parts, ", ")
	if len(parts) == 0 {
		out = "hook trust: no hooks listed"
	}
	if h.Err != nil {
		out += " (not set: " + h.Err.Error() + ")"
	}
	return out
}

var (
	codexTrustMu    sync.Mutex
	codexTrustNamed = map[string]bool{}
)

// codexNamesHookTrust reports whether the Codex executable at path names trusted_hash, the
// field a hook's trust is recorded in: a build without it has no hook trust, and no app-server
// is started to ask.
func codexNamesHookTrust(path string) bool {
	codexTrustMu.Lock()
	defer codexTrustMu.Unlock()
	if named, ok := codexTrustNamed[path]; ok {
		return named
	}
	data, err := os.ReadFile(path)
	named := err == nil && bytes.Contains(data, []byte("trusted_hash"))
	codexTrustNamed[path] = named
	return named
}

// trustCodexHooks trusts every hook sb's Codex finds for its working directory, the way Codex
// records a developer's review of them (/hooks in the TUI, Settings → Hooks in Desktop): it
// asks the build's own app-server (hooks/list, with config's -c options, so the hooks feature
// is on as for the run) for each hook's key and current hash, writes each into config.toml as
// [hooks.state."<key>"] trusted_hash, and lists them again to see Codex now trusts them.
// Codex computes the hash; a hook rewritten after this is untrusted again.
func trustCodexHooks(t *testing.T, sb *Sandbox, config []string) CodexHookTrust {
	t.Helper()
	if !codexNamesHookTrust(sb.Codex.Path) {
		return CodexHookTrust{}
	}
	args := []string{"app-server"}
	for i := 0; i+1 < len(config); i++ {
		if config[i] == "-c" {
			args = append(args, config[i], config[i+1])
			i++
		}
	}
	// What the app-server makes of CODEX_HOME on its way (its databases, installation id,
	// bundled skills) goes with it: the run finds the home as it would have, the trust aside.
	before := pathsUnder(sb.CodexHome)
	before[sb.CodexHome], before[filepath.Join(sb.CodexHome, "config.toml")] = true, true
	a, err := launchAppServer(t, sb.Codex.Path, args, sb.workDir(), sb.codexEnv(RouteAPIKey), "terma-e2e")
	if err != nil {
		return CodexHookTrust{Gated: true, Err: err}
	}
	defer func() {
		a.Close()
		removeAddedPaths(sb.CodexHome, before)
	}()
	list := func() ([]codexListedHook, error) {
		res, err := a.tryCall("hooks/list", map[string]any{"cwds": []string{sb.workDir()}})
		return listedHooks(res), err
	}
	hooks, err := list()
	if err != nil {
		return CodexHookTrust{Gated: true, Err: err}
	}
	if len(hooks) == 0 {
		return CodexHookTrust{Gated: true, Statuses: map[string]int{}, Err: fmt.Errorf("Codex listed no hooks for %s", sb.workDir())}
	}
	trust := CodexHookTrust{}
	hashes := map[string]string{}
	for _, h := range hooks {
		trust.Gated = trust.Gated || h.Status != ""
		if h.Status != "" && !h.Managed && !strings.EqualFold(h.Status, "trusted") {
			hashes[h.Key] = h.Hash
		}
	}
	if !trust.Gated {
		return trust
	}
	if len(hashes) > 0 {
		cfg := filepath.Join(sb.CodexHome, "config.toml")
		existing, _ := os.ReadFile(cfg)
		if err := os.WriteFile(cfg, []byte(withCodexHookTrust(string(existing), hashes)), 0o600); err != nil {
			t.Fatal(err)
		}
		if hooks, err = list(); err != nil {
			trust.Err = err
		}
	}
	trust.Statuses = map[string]int{}
	for _, h := range hooks {
		trust.Statuses[strings.ToLower(h.Status)]++
	}
	if trust.Err == nil && !trust.Trusted() {
		trust.Err = fmt.Errorf("Codex does not trust the hooks its hooks/list hashed once their hashes are in %s", filepath.Join(sb.CodexHome, "config.toml"))
	}
	return trust
}

// pathsUnder is every path under root, root's own included.
func pathsUnder(root string) map[string]bool {
	out := map[string]bool{}
	_ = filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		if err == nil {
			out[p] = true
		}
		return nil
	})
	return out
}

// removeAddedPaths removes from root whatever is not in before, a folder new since whole.
func removeAddedPaths(root string, before map[string]bool) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || before[p] {
			return nil
		}
		_ = os.RemoveAll(p)
		if d.IsDir() {
			return fs.SkipDir
		}
		return nil
	})
}

// withCodexHookTrust is config with each of hashes' keys trusted at its hash: its
// [hooks.state."<key>"] table's trusted_hash replaced, the table's other settings (enabled)
// kept, or a table of its own appended.
func withCodexHookTrust(config string, hashes map[string]string) string {
	var out []string
	done := map[string]bool{}
	key := ""
	for line := range strings.Lines(config) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			key = ""
			if q, ok := strings.CutPrefix(trimmed, "[hooks.state."); ok {
				if k, err := strconv.Unquote(strings.TrimSuffix(q, "]")); err == nil && hashes[k] != "" && !done[k] {
					key = k
				}
			}
			if !strings.HasSuffix(line, "\n") {
				line += "\n"
			}
			out = append(out, line)
			if key != "" {
				out = append(out, "trusted_hash = "+tomlQuote(hashes[key])+"\n")
				done[key] = true
			}
			continue
		}
		if f := strings.Fields(strings.ReplaceAll(trimmed, "=", " = ")); key != "" && len(f) > 1 && f[0] == "trusted_hash" && f[1] == "=" {
			continue
		}
		out = append(out, line)
	}
	if len(out) > 0 && !strings.HasSuffix(out[len(out)-1], "\n") {
		out[len(out)-1] += "\n"
	}
	for _, k := range slices.Sorted(maps.Keys(hashes)) {
		if !done[k] {
			out = append(out, "\n[hooks.state."+tomlQuote(k)+"]\ntrusted_hash = "+tomlQuote(hashes[k])+"\n")
		}
	}
	return strings.Join(out, "")
}

// trustedHashes reads each [hooks.state."<key>"] table's trusted_hash from config text.
func trustedHashes(config string) map[string]string {
	out := map[string]string{}
	key := ""
	for line := range strings.Lines(config) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			key = ""
			if q, ok := strings.CutPrefix(line, "[hooks.state."); ok {
				key, _ = strconv.Unquote(strings.TrimSuffix(q, "]"))
			}
			continue
		}
		if v, ok := strings.CutPrefix(line, "trusted_hash"); ok && key != "" {
			if v, ok = strings.CutPrefix(strings.TrimSpace(v), "="); ok {
				out[key], _ = strconv.Unquote(strings.TrimSpace(v))
			}
		}
	}
	return out
}

// CodexDaemon is a sandbox's own app-server daemon, listening where Codex's clients
// look for one ($CODEX_HOME/app-server-control/app-server-control.sock).
type CodexDaemon struct {
	cmd    *exec.Cmd
	stderr strings.Builder
	once   sync.Once
}

// PID is the daemon's process.
func (d *CodexDaemon) PID() int { return d.cmd.Process.Pid }

// Stop ends the daemon as a service manager would (SIGTERM), so it flushes what its
// exporters hold, and waits for it.
func (d *CodexDaemon) Stop() {
	d.once.Do(func() {
		_ = d.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = d.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = d.cmd.Process.Kill()
			<-done
		}
	})
}

// UseCodexConfigFile moves what the tests otherwise pass as -c overrides into the
// sandbox's config.toml: a TUI given any override runs in-process instead of in the
// daemon ("remove configuration overrides"), so a daemon-attached TUI needs none.
func (sb *Sandbox) UseCodexConfigFile(providerURL string) {
	t := sb.T
	t.Helper()
	top := `cli_auth_credentials_store = "file"
model_provider = "telemetry_fixture"
model_reasoning_effort = "low"
sandbox_mode = "workspace-write"

[features]
plugins = false
remote_plugin = false

[model_providers.telemetry_fixture]
name = "Telemetry fixture"
base_url = ` + tomlQuote(providerURL) + `
wire_api = "responses"
requires_openai_auth = false
`
	cfg := filepath.Join(sb.CodexHome, "config.toml")
	existing, _ := os.ReadFile(cfg)
	// Top-level keys first: after a [table] they would belong to it.
	if err := os.WriteFile(cfg, append([]byte(top+"\n"), existing...), 0o600); err != nil {
		t.Fatal(err)
	}
}

// UseShortCodexHome moves the sandbox's CODEX_HOME to a short path. Codex's clients
// reach the daemon through $CODEX_HOME/app-server-control/app-server-control.sock, and
// a socket path is bounded (SUN_LEN, 104 bytes on macOS): a test's temporary directory
// is too long ("path must be shorter than SUN_LEN"). It must run before anything
// writes Codex's configuration.
func (sb *Sandbox) UseShortCodexHome() {
	t := sb.T
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "tcx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if old, err := os.ReadDir(sb.CodexHome); err == nil {
		for _, e := range old {
			if data, err := os.ReadFile(filepath.Join(sb.CodexHome, e.Name())); err == nil && !e.IsDir() {
				_ = os.WriteFile(filepath.Join(dir, e.Name()), data, 0o600)
			}
		}
	}
	sb.CodexHome = dir
}

// StartCodexDaemon runs `codex app-server --listen unix://` for the sandbox's
// CODEX_HOME — the daemon a TUI attaches to when one is running (Codex 0.157+) — and
// waits for its control socket.
func (sb *Sandbox) StartCodexDaemon(b Binary) *CodexDaemon {
	t := sb.T
	t.Helper()
	d := &CodexDaemon{cmd: exec.Command(b.Path, "app-server", "--listen", "unix://")}
	d.cmd.Dir = sb.Dir
	d.cmd.Env = sb.codexEnv(RouteAPIKey)
	d.cmd.Stderr = &d.stderr
	if err := d.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Stop)
	sock := filepath.Join(sb.CodexHome, "app-server-control", "app-server-control.sock")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			return d
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the Codex daemon never listened at %s:\n%s", sock, tail(d.stderr.String(), 2000))
	return nil
}

// CodexDaemonTUI runs the interactive TUI in dir with no overrides, so it attaches to
// the running daemon, for one prompt; it returns once the reply is on screen and the
// session has had time to title itself.
func (sb *Sandbox) CodexDaemonTUI(b Binary, dir, prompt string) {
	t := sb.T
	t.Helper()
	term, err := Start(dir, sb.codexEnv(RouteAPIKey), 40, 140, b.Path, "-C", dir, prompt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := term.Expect(regexp.MustCompile(`TERMA_TELEMETRY_REPLY`), 90*time.Second); err != nil {
		t.Fatalf("the daemon-attached TUI never replied:\n%s", tail(term.Text(), 3000))
	}
	time.Sleep(12 * time.Second) // the title conversation starts after the first reply
	_ = term.Close("/quit", 8*time.Second)
}
