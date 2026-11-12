package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The desktop apps' profiles (desktop.go): each release of Claude Desktop and of the Codex app
// is run whole, as a person runs it, for one first turn against the fixture provider, and its
// profile is keyed by the app's own version. What the app's agent sends the model, the agent's
// command line and environment as the app started it, the app's and the agent's requests to
// anything else, what they wrote, and both executables and their signatures are recorded. How
// the run differed from a person's at the app is in the profile's notes, and from there in the
// snapshot's.

// claudeDesktopManaged is where Claude Desktop reads an organization's managed preferences.
const claudeDesktopManaged = "/Library/Managed Preferences/com.anthropic.claudefordesktop.plist"

// TestClaudeDesktopProfile runs each Claude Desktop release's Code tab for one turn. Desktop
// needs either a claude.ai account or an organization's third-party inference: it runs here in
// the latter, set up as an organization sets it up (managed preferences naming an inference
// gateway, the fixture provider, and an egress proxy, the recording proxy), so no account is
// needed. The session is started through the app's window: the welcome screen's Continue,
// then a New Claude Code Session link with the prompt and the repository, and Return.
func TestClaudeDesktopProfile(t *testing.T) {
	forEachDesktopApp(t, "claude-desktop", func(t *testing.T, app DesktopApp) {
		track(t)
		pin, err := claudeDesktopPinOf(app)
		if err != nil {
			t.Fatal(err)
		}
		platform := pin.Manifest.Platforms["darwin-arm64"]
		home, _ := os.UserHomeDir()
		userData := filepath.Join(home, "Library", "Application Support", "Claude-3p")
		sb := New(t, Isolated, CleanRoom())
		b := Binary{Harness: app.Harness, Version: app.Version, Path: app.Executable()}
		desktopPlaceholders(t, b, app, sb, home)
		Placeholder(t, b, userData, "<app-data>")
		Placeholder(t, b, filepath.Join(home, ".claude"), "<claude-config>")

		proxy := StartEgressProxy(t)
		// The Claude Code build the release pins, which Desktop downloads on its first session:
		// fetched here from the same URL, checked against the same checksum, and served to the
		// app by the proxy.
		agentURL, checksum := fmt.Sprintf("%s/%s/darwin-arm64/%s.zst", pin.BaseURL, pin.Version, platform.Binary), platform.Checksum
		if platform.Bundle != nil {
			agentURL, checksum = fmt.Sprintf("%s/%s/darwin-arm64/claude.app.tar.zst", pin.BaseURL, pin.Version), platform.Bundle.Checksum
		}
		data, err := fetch(agentURL)
		if err != nil {
			t.Fatalf("fetch the Claude Code build %s pins: %v", app.Version, err)
		}
		if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != checksum {
			t.Fatalf("%s does not match the checksum %s pins", agentURL, app.Version)
		}
		u, _ := url.Parse(agentURL)
		proxy.Serve(u.Host, u.Path, data)
		trustProxyCA(t, proxy)

		var calls atomic.Int32
		// The app asks its gateway itself too (a connection probe, a session's title): the turn is
		// the agent's, Claude Code's requests.
		var agentCalls atomic.Int32
		provider := httptest.NewServer(proxy.Provider(isClaudeModelCall, CaptureProvider(t, b,
			countAgent(&agentCalls, "claude-cli/", gatewayModels(claudeWorkloadProvider(&calls, nil, false))))))
		defer provider.Close()
		Placeholder(t, b, provider.URL, "<provider>")
		sudoWrite(t, claudeDesktopManaged, plistXML(map[string]any{
			"inferenceProvider":       "gateway",
			"inferenceGatewayBaseUrl": provider.URL,
			"inferenceGatewayApiKey":  "synthetic-gateway-key",
			"egressProxyUrl":          proxy.URL,
		}))
		// The recorder, for every hook event the pinned build knows (read from the same build as
		// its CLI distribution ships it), in the user's settings, as a person's own hooks are.
		cli, err := ensureClaude(pin.Version)
		if err != nil {
			t.Fatalf("fetch Claude Code %s to read its hook events: %v", pin.Version, err)
		}
		events, source, _ := HookEventsOf(Binary{Harness: "claude", Version: pin.Version, Path: cli})
		rec := filepath.Join(sb.Dir, "hook-recorder")
		desktopHooks(t, "claude", events, filepath.Join(home, ".claude", "settings.json"), rec)
		storage := claudeDesktopStorage(home, userData, sb)
		before := SnapshotFiles(storage.Roots...)

		r := LaunchDesktopApp(t, app, nil)
		r.Snap("1-launched")
		if !r.Press("Continue", 90*time.Second) {
			r.Snap("2-no-continue")
			t.Log("no Continue on Desktop's welcome screen; going on")
		}
		time.Sleep(5 * time.Second)
		r.Snap("2-welcomed")
		link := "claude://code/new?" + url.Values{"q": {profilePrompt}, "folder": {sb.Repo}}.Encode()
		if out, err := desktopCmd("open", link).CombinedOutput(); err != nil {
			t.Fatalf("open %s: %v: %s", link, err, out)
		}
		time.Sleep(15 * time.Second)
		r.Snap("3-new-session")
		// A folder new to Desktop is one it asks to trust, as it asks a person.
		if r.Press("Trust workspace", 30*time.Second) {
			time.Sleep(3 * time.Second)
			r.Snap("3-trusted")
		}
		// Send, once the app has the folder ready; a press it was not ready for is pressed again.
		for try := 1; agentCalls.Load() == 0 && try <= 3; try++ {
			if !r.Press("Send", 60*time.Second) {
				r.Key("return")
			}
			waitCalls(&agentCalls, 30*time.Second, func(int) {})
			r.Snap(fmt.Sprintf("4-sent-%d", try))
		}
		if !waitCalls(&agentCalls, 3*time.Minute, func(n int) {
			r.Snap(fmt.Sprintf("4-waiting-%d", n))
		}) {
			r.Keep(filepath.Join(home, "Library", "Logs", "Claude-3p"))
			t.Fatalf("Desktop's Code session sent its provider nothing:\n%s", tail(r.Window(), 4000))
		}
		time.Sleep(15 * time.Second) // what follows the reply: the session's title, its idle
		r.Snap("5-replied")
		r.Keep(filepath.Join(home, "Library", "Logs", "Claude-3p"))

		agent, agentPath := desktopAgent(r, func(argv []string) bool {
			return strings.Contains(argv[0], "/claude-code/") && filepath.Base(argv[0]) == "claude"
		})
		if agent == 0 {
			t.Fatalf("no Claude Code process under Desktop:\n%v", descendants(r.PID))
		}
		launch, err := launchOf(agent, r.PID, desktopPlaces(app, home))
		if err != nil {
			t.Fatalf("read how Desktop started Claude Code: %v", err)
		}
		RecordLaunch(t, b, launch)
		RecordCommandOf(t, b, "claude", launch.Argv[1:])
		_, started, _ := procArgs(agent)
		recordDesktopHooks(t, b, events, source, rec, started, sb.Dir, "")
		Placeholder(t, b, agentPath, "<agent>")
		time.Sleep(egressSettle)
		proxy.Record(t, b)
		r.Quit()
		RecordStorage(t, b, before, storage)

		RecordDesktopFacts(t, b, app, AgentFacts{Harness: "claude", Version: pin.Version, Path: agentPath, Source: "downloaded",
			Download: agentURL, Checksum: checksum, SDK: "@anthropic-ai/claude-agent-sdk " + pin.SDKWrapperVersion})
		RecordHelpTree(t, Binary{Harness: b.Harness, Version: b.Version, Path: agentPath}, helpEnv(sb), "claude")
		RecordNote(t, b, "Claude Desktop ran whole on a hosted macOS runner, in its third-party inference mode as an "+
			"organization sets it up (managed preferences: an inference gateway, the fixture provider, and an egress proxy, the recording "+
			"proxy), with no claude.ai account: its Code tab ran Claude Code "+pin.Version+", the build the release pins, through the Agent SDK "+
			pin.SDKWrapperVersion+". Signed in with claude.ai, Desktop runs the same build with CLAUDE_CODE_ENTRYPOINT=claude-desktop and the "+
			"account's OAuth token where this run has claude-desktop-3p and the gateway.")
		RecordNote(t, b, loginNote())
		RecordNote(t, b, "The session was started through the app's window, through the accessibility API: the welcome screen's Continue, "+
			"a New Claude Code Session link (claude://code/new) with the prompt and the repository, the folder's Trust workspace, and Send.")
		RecordNote(t, b, "The fixture answered the gateway's model list (GET /v1/models, which Desktop asks before it offers a session) "+
			"with the three models the aliases of Claude Code "+pin.Version+" resolve to: claude-opus-5-5, claude-sonnet-5-5, claude-haiku-5-5.")
		RecordNote(t, b, "The app reached everything but the model through the recording proxy, whose CA the machine trusted, as a company's "+
			"inspecting proxy is: each request was answered locally and empty, but the Claude Code build Desktop downloads on its first "+
			"session, which was a copy fetched from the same URL and checked against the checksum the release pins.")
	})
}

// TestCodexDesktopProfile runs each Codex app release for one thread's first turn. The app runs
// its threads in the `codex app-server` it bundles; signed in with an API key and pointed at the
// fixture provider in its config.toml (no ChatGPT account), with the repository as its project, it
// is typed into as a person types: the prompt into its composer, then Return, through its
// DevTools protocol.
func TestCodexDesktopProfile(t *testing.T) {
	forEachDesktopApp(t, "codex-desktop", func(t *testing.T, app DesktopApp) {
		track(t)
		pkg, codexPath, err := codexDesktopCLI(app)
		if err != nil {
			t.Fatalf("the Codex %s bundles: %v", app.Version, err)
		}
		home, _ := os.UserHomeDir()
		codexHome := filepath.Join(home, ".codex")
		sb := New(t, Isolated, CleanRoom())
		b := Binary{Harness: app.Harness, Version: app.Version, Path: app.Executable()}
		desktopPlaceholders(t, b, app, sb, home)
		Placeholder(t, b, codexHome, "<codex-home>")
		Placeholder(t, b, filepath.Join(home, "Library", "Application Support", "Codex"), "<app-data>")
		Placeholder(t, b, codexPath, "<agent>")
		asideHome(t, codexHome)

		proxy := StartEgressProxy(t)
		trustProxyCA(t, proxy)
		var calls atomic.Int32
		provider := httptest.NewServer(proxy.Provider(isCodexModelCall, CaptureProvider(t, b, codexWorkloadProvider(&calls, nil))))
		defer provider.Close()
		Placeholder(t, b, provider.URL, "<provider>")
		writeFiles(t, map[string]string{
			filepath.Join(codexHome, "config.toml"): `model_provider = "fixture"

[model_providers.fixture]
name = "Fixture"
base_url = ` + tomlQuote(provider.URL) + `
wire_api = "responses"
requires_openai_auth = false

[projects.` + tomlQuote(sb.Repo) + `]
trust_level = "trusted"
`,
			filepath.Join(codexHome, "auth.json"):                `{"OPENAI_API_KEY":"synthetic-telemetry-key"}`,
			filepath.Join(codexHome, ".codex-global-state.json"): mustJSON(map[string]any{"electron-saved-workspace-roots": []string{sb.Repo}, "active-workspace-roots": []string{sb.Repo}}),
		})
		storage := StorageOptions{
			Roots:   []StorageRoot{{codexHome, "<codex-home>"}, {sb.Repo, "<repo>"}},
			Replace: sb.storageReplace(""),
			Exclude: []string{"<codex-home>/config.toml", "<codex-home>/auth.json", "<codex-home>/hooks.json"},
			Kinds:   []StorageKind{{regexp.MustCompile(`^<codex-home>/(archived_)?sessions/.*\.jsonl$`), "transcript"}},
		}
		// The recorder, for every hook event the bundled Codex knows, in the user's hooks.json, and
		// trusted as the app's Settings → Hooks trusts a person's.
		events, source, _ := HookEventsOf(Binary{Harness: "codex", Version: pkg.Version, Path: codexPath})
		rec := filepath.Join(sb.Dir, "hook-recorder")
		desktopHooks(t, "codex", events, filepath.Join(codexHome, "hooks.json"), rec)
		trust := trustCodexDesktopHooks(t, codexPath, codexHome, sb.Repo)
		before := SnapshotFiles(storage.Roots...)

		port := freePort(t)
		var env []string
		for _, kv := range proxy.Env() {
			if k, _, _ := strings.Cut(kv, "="); slices.Contains([]string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", "CODEX_CA_CERTIFICATE"}, k) {
				env = append(env, kv)
			}
		}
		r := LaunchDesktopApp(t, app, env, fmt.Sprintf("--remote-debugging-port=%d", port), "--proxy-server="+proxy.URL)
		page, err := DialCDP(port, func(u string) bool { return strings.HasPrefix(u, "app://") }, 90*time.Second)
		if err != nil {
			r.Snap("1-no-page")
			t.Fatal(err)
		}
		defer page.Close()
		composer := `(() => { const e = document.querySelector('[contenteditable=true], [role=textbox], textarea'); if (!e) return false; e.focus(); return true; })()`
		deadline := time.Now().Add(90 * time.Second)
		for ok := false; !ok; {
			v, _ := page.Eval(composer)
			ok, _ = v.(bool)
			if !ok && time.Now().After(deadline) {
				r.Snap("1-no-composer")
				t.Fatalf("the Codex app showed no composer:\n%s", tail(page.Text(), 3000))
			}
			if !ok {
				time.Sleep(2 * time.Second)
			}
		}
		r.Snap("1-composer")
		if err := page.Type(profilePrompt); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		if err := page.Enter(); err != nil {
			t.Fatal(err)
		}
		if !waitCalls(&calls, 3*time.Minute, func(n int) { r.Snap(fmt.Sprintf("2-waiting-%d", n)) }) {
			t.Fatalf("the Codex app's thread sent its provider nothing:\n%s", tail(page.Text(), 3000))
		}
		if !page.WaitText("TERMA_TELEMETRY_REPLY", 2*time.Minute) {
			t.Errorf("the Codex app never showed the reply:\n%s", tail(page.Text(), 3000))
		}
		time.Sleep(10 * time.Second)
		r.Snap("3-replied")

		server, _ := desktopAgent(r, func(argv []string) bool {
			return filepath.Base(argv[0]) == "codex" && slices.Contains(argv, "app-server")
		})
		if server == 0 {
			t.Fatalf("no app-server under the Codex app:\n%v", descendants(r.PID))
		}
		launch, err := launchOf(server, r.PID, desktopPlaces(app, home))
		if err != nil {
			t.Fatalf("read how the Codex app started its app-server: %v", err)
		}
		RecordLaunch(t, b, launch)
		RecordCommandOf(t, b, "codex", launch.Argv[1:])
		_, started, _ := procArgs(server)
		recordDesktopHooks(t, b, events, source, rec, started, sb.Dir, trust)
		time.Sleep(egressSettle)
		proxy.Record(t, b)
		r.Quit()
		RecordStorage(t, b, before, storage)

		RecordDesktopFacts(t, b, app, AgentFacts{Harness: "codex", Version: pkg.Version, Path: codexPath, Source: "bundled"})
		helpBin := Binary{Harness: b.Harness, Version: b.Version, Path: codexPath}
		RecordHelpTree(t, helpBin, helpEnv(sb), "codex")
		RecordAppServerSurface(t, b, codexPath, helpEnv(sb))
		RecordNote(t, b, "The Codex app ran whole on a hosted macOS runner, signed in with an API key (auth.json) and its model "+
			"provider the fixture provider (config.toml's model_provider), with no ChatGPT account; the repository was its project "+
			"(the app's saved workspace roots) and trusted (projects.<repo>.trust_level). It ran its thread in the app-server of the Codex "+
			pkg.Version+" it bundles.")
		RecordNote(t, b, loginNote())
		RecordNote(t, b, "The thread was started through the app's window: the prompt typed into its composer and Return, through the "+
			"app's DevTools protocol, which a person's launch does not open (--remote-debugging-port).")
		RecordNote(t, b, "The app reached everything but the model through the recording proxy (its window's through --proxy-server, its "+
			"app-server's through HTTPS_PROXY, trusting the proxy's CA through CODEX_CA_CERTIFICATE): each request was answered locally "+
			"and empty.")
	})
}

// countAgent counts the model requests the agent sent, by its user agent's prefix, before
// next answers them.
func countAgent(n *atomic.Int32, agent string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isClaudeModelCall(r) && strings.HasPrefix(r.UserAgent(), agent) {
			n.Add(1)
		}
		next.ServeHTTP(w, r)
	})
}

// gatewayModels answers an inference gateway's model list (GET /v1/models, which Claude
// Desktop asks its gateway for before it offers a session, and which a gateway forwards to
// Anthropic's API) as that API does, with the models the aliases of the Claude Code build the
// release pins resolve to (the reference's own snapshot of that build); everything else goes to
// next.
func gatewayModels(next http.Handler) http.Handler {
	models := []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5"}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			next.ServeHTTP(w, r)
			return
		}
		var data []any
		for _, id := range models {
			data = append(data, map[string]any{"type": "model", "id": id, "display_name": id, "created_at": "2026-01-01T00:00:00Z"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "has_more": false, "first_id": models[0], "last_id": models[len(models)-1]})
	})
}

// desktopPlaceholders are the placeholders of a desktop app's run: the machine's home, the
// sandbox's repository, the app bundle and the prompt.
func desktopPlaceholders(t *testing.T, b Binary, app DesktopApp, sb *Sandbox, home string) {
	t.Helper()
	Placeholder(t, b, home, "<home>")
	Placeholder(t, b, sb.Repo, "<repo>")
	Placeholder(t, b, sb.Dir, "<sandbox>")
	Placeholder(t, b, app.Bundle, "<app>")
	Placeholder(t, b, filepath.Dir(app.Bundle), "<app-dir>")
	Placeholder(t, b, profilePrompt, "<prompt>")
	if real, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
		Placeholder(t, b, real, "<tmp>")
	}
}

// claudeDesktopStorage is where Desktop's Code sessions write: Claude Code's config folder, and
// the app's own record of its sessions.
func claudeDesktopStorage(home, userData string, sb *Sandbox) StorageOptions {
	return StorageOptions{
		Roots: []StorageRoot{{filepath.Join(home, ".claude"), "<claude-config>"},
			{filepath.Join(userData, "claude-code-sessions"), "<app-data>/claude-code-sessions"}, {sb.Repo, "<repo>"}},
		Replace: sb.storageReplace(""),
		Exclude: []string{"<claude-config>/settings.json"},
		Kinds: []StorageKind{
			{regexp.MustCompile(`^<claude-config>/projects/.*\.jsonl$`), "transcript"},
			{regexp.MustCompile(`^<app-data>/claude-code-sessions/`), "app-sessions"},
		},
	}
}

// desktopAgent is the process under the app whose command line is, by is, the agent, and its
// executable.
func desktopAgent(r *RunningApp, is func(argv []string) bool) (int, string) {
	for _, p := range descendants(r.PID) {
		argv, _, err := procArgs(p.PID)
		if err == nil && len(argv) > 0 && is(argv) {
			return p.PID, argv[0]
		}
	}
	return 0, ""
}

// waitCalls waits up to timeout for the provider's first model call, calling each every 30
// seconds it waits.
func waitCalls(calls *atomic.Int32, timeout time.Duration, each func(n int)) bool {
	deadline := time.Now().Add(timeout)
	for n := 1; time.Now().Before(deadline); n++ {
		for range 30 {
			if calls.Load() > 0 {
				return true
			}
			time.Sleep(time.Second)
		}
		each(n)
	}
	return calls.Load() > 0
}

// helpEnv is what an agent's help runs with: the sandbox's home, nothing of the machine's.
func helpEnv(sb *Sandbox) []string {
	return []string{"HOME=" + sb.Home, "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "CODEX_HOME=" + sb.CodexHome, "CLAUDE_CONFIG_DIR=" + sb.ClaudeConfig}
}

// RecordCommandOf records the command line t's profile of b ran as program's, the prompt left out.
func RecordCommandOf(t *testing.T, b Binary, program string, args []string) {
	t.Helper()
	p := profileOf(t, b)
	profilesMu.Lock()
	defer profilesMu.Unlock()
	p.Command = append([]string{program}, args...)
}

// RecordDesktopFacts records the app bundle's facts and its agent's executable's, and who
// signed each: the agent's signature as the profile's signing, the app's in its facts.
func RecordDesktopFacts(t *testing.T, b Binary, app DesktopApp, agent AgentFacts) {
	t.Helper()
	facts, err := BinaryFactsOf(Binary{Harness: agentOf(b.Harness), Version: agent.Version, Path: agent.Path})
	if err != nil {
		t.Logf("binary facts of %s's agent: %v", app.Name, err)
	}
	if s, err := inspectSigning(agent.Path); err == nil && s != nil {
		RecordProbe(t, b, "signing", s)
	}
	RecordBinary(t, b, DesktopFacts{BinaryFacts: facts, Agent: agent, App: AppFacts{Name: app.Name, BundleID: app.BundleID, Version: app.Version,
		Build: app.Build, Download: app.URL, SizeBytes: dirSize(app.Bundle), Signing: app.Signing}})
}

// RecordAppServerSurface records the JSON-RPC methods the app-server at codex serves and sends
// (its generate-json-schema), as the "app-server" probe.
func RecordAppServerSurface(t *testing.T, b Binary, codex string, env []string) {
	t.Helper()
	out := t.TempDir()
	cmd := exec.Command(codex, "app-server", "generate-json-schema", "--out", out)
	cmd.Env = env
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Logf("codex app-server generate-json-schema: %v: %s", err, data)
		return
	}
	RecordProbe(t, b, "app-server", AppServerSurfaceOf(out))
}

// AppServerSurfaceOf reads a generate-json-schema bundle's methods, by the schema file that
// names them ("ClientRequest", "ServerNotification", …).
func AppServerSurfaceOf(dir string) map[string][]string {
	surface := map[string][]string{}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".json") {
			return nil
		}
		kind := strings.TrimSuffix(filepath.Base(p), ".json")
		if !slices.Contains([]string{"ClientRequest", "ClientNotification", "ServerRequest", "ServerNotification"}, kind) {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		var doc any
		if json.Unmarshal(data, &doc) != nil {
			return nil
		}
		surface[kind] = methodsIn(doc)
		return nil
	})
	return surface
}

// methodsIn are the method names a schema's variants fix (properties.method's enum or const).
func methodsIn(doc any) []string {
	seen := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if props, ok := x["properties"].(map[string]any); ok {
				if m, ok := props["method"].(map[string]any); ok {
					if c, ok := m["const"].(string); ok {
						seen[c] = true
					}
					if e, ok := m["enum"].([]any); ok {
						for _, s := range e {
							if s, ok := s.(string); ok {
								seen[s] = true
							}
						}
					}
				}
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
	walk(doc)
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// asideHome moves dir away for t, if it is there, and puts it back after.
func asideHome(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(dir); err != nil {
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		return
	}
	aside := dir + ".terma-aside"
	if err := os.Rename(dir, aside); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
		_ = os.Rename(aside, dir)
	})
}

// freePort is a loopback port nothing listens on.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
