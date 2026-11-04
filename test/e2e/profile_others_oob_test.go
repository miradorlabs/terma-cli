package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// The out-of-band captures of the harnesses with no plain profile turn here: one scenario each,
// Test<Harness>ProfileEgress, runs one turn of the build's defaults behind the recording proxy,
// with what the sandbox's own setup turns off for its other scenarios turned back on (Gemini's
// usage statistics and update checks, OpenCode's autoupdate and sharing, DeepSeek's session
// telemetry, Pi's --offline), and none of terma's plugins, whose traffic is terma's. The same
// turn's files, the hooks a harness runs itself (Gemini's), and the build's executable and help
// are recorded beside its egress. Copilot CLI, which only runs against GitHub's service, records
// its executable and help alone (TestCopilotProfileBinary).

// otherProfile is how one harness's out-of-band profile turn is taken.
type otherProfile struct {
	program     string
	isModelCall func(*http.Request) bool
	provider    func(calls *atomic.Int32) http.Handler
	// setup points sb's harness at the provider at url, as its defaults otherwise have it.
	setup func(sb *Sandbox, url string)
	// run takes the turn and returns the command line it ran, the prompt left out.
	run func(t *testing.T, sb *Sandbox, b Binary) []string
	// exclude are the files setup wrote, as StorageOptions name them.
	exclude []string
	kinds   []StorageKind
	rewrite []StorageRewrite
	hooks   bool
}

func profileOtherOutOfBand(t *testing.T, b Binary, h otherProfile) {
	t.Helper()
	track(t)
	sb := New(t, Isolated)
	proxy := StartEgressProxy(t)
	var calls atomic.Int32
	provider := httptest.NewServer(proxy.Provider(h.isModelCall, CaptureProvider(t, b, h.provider(&calls))))
	defer provider.Close()
	h.setup(sb, provider.URL)
	sb.unproxyHooks()
	PlaceSandbox(t, b, sb)
	Placeholder(t, b, provider.URL, "<provider>")
	Placeholder(t, b, profilePrompt, "<prompt>")
	storage := StorageOptions{Roots: []StorageRoot{{sb.Home, "<home>"}, {sb.Repo, "<repo>"}},
		Replace: sb.storageReplace(""), Exclude: h.exclude, Kinds: h.kinds, Rewrite: h.rewrite}
	before := SnapshotFiles(storage.Roots...)
	env := sb.ExtraEnv
	sb.ExtraEnv = append(slices.Clone(env), proxy.Env()...)
	args := h.run(t, sb, b)
	sb.ExtraEnv = env
	time.Sleep(egressSettle)
	proxy.Record(t, b)
	RecordCommand(t, b, args)
	if calls.Load() == 0 {
		t.Errorf("%s sent its provider no model request", b.Harness)
	}
	RecordStorage(t, b, before, storage)
	if h.hooks {
		RecordHooks(t, b, sb)
	}
	RecordBinaryFacts(t, b)
	RecordHelpTree(t, b, sb.termaEnv(), h.program)
}

// runProfileCommand runs b with args in dir, in env, and fails t if it fails.
func runProfileCommand(t *testing.T, b Binary, dir string, env []string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.Path, args...)
	cmd.Dir = dir
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", b.Harness, strings.Join(args, " "), err, tail(string(out), 3000))
	}
}

// editJSON rewrites the JSON object at path with edit.
func editJSON(t *testing.T, path string, edit func(doc map[string]any)) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	edit(doc)
	data, _ = json.MarshalIndent(doc, "", "  ")
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func isChatCompletion(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat/completions")
}

func TestGeminiProfileEgress(t *testing.T) {
	forEachGemini(t, func(t *testing.T, b Binary) {
		profileOtherOutOfBand(t, b, otherProfile{
			program:     "gemini",
			isModelCall: func(r *http.Request) bool { return strings.Contains(r.URL.Path, "enerateContent") },
			provider:    func(calls *atomic.Int32) http.Handler { return geminiProvider(calls, "", nil) },
			setup: func(sb *Sandbox, url string) {
				sb.UseGemini(url)
				// The API-key login stays; usage statistics and update checks are Gemini's defaults.
				editJSON(t, filepath.Join(sb.Home, ".gemini", "settings.json"), func(doc map[string]any) {
					delete(doc, "privacy")
					delete(doc, "general")
				})
				sb.geminiHooksExtension()
			},
			run: func(t *testing.T, sb *Sandbox, b Binary) []string {
				args := []string{"-p", profilePrompt, "--output-format", "json"}
				runProfileCommand(t, b, sb.Repo, sb.termaEnv(), args...)
				return []string{"--output-format", "json"}
			},
			exclude: []string{"<home>/.gemini/settings.json", "<home>/.gemini/trustedFolders.json"},
			kinds: []StorageKind{
				{regexp.MustCompile(`/chats/`), "transcript"},
				{regexp.MustCompile(`^<home>/\.gemini/tmp/[^/]+/logs\.json$`), "history"},
			},
			// A chat's file ends in the first eight hex digits of its session's id.
			rewrite: []StorageRewrite{{regexp.MustCompile(`^(session-<time>-)[0-9a-f]{8}(\.jsonl?)$`), "${1}<id>${2}"}},
			hooks:   true,
		})
	})
}

// geminiHooksExtension writes terma's Gemini extension, its hooks calling the recording shim,
// as terma's setup has them call terma.
func (sb *Sandbox) geminiHooksExtension() {
	shim := filepath.Join(strings.TrimSuffix(sb.binDir(), ":"), "terma")
	hooks := map[string]any{}
	for _, e := range [][2]string{{"SessionStart", "gemini-session-start"}, {"BeforeAgent", "gemini-prompt"}, {"AfterTool", "gemini-after-tool"}, {"SessionEnd", "gemini-session-end"}} {
		hooks[e[0]] = []any{map[string]any{"matcher": "*", "hooks": []any{map[string]any{"type": "command", "command": "'" + shim + "' 'hook' " + e[1]}}}}
	}
	data, _ := json.Marshal(map[string]any{"hooks": hooks})
	ext := filepath.Join(sb.Home, ".gemini", "extensions", "terma")
	sb.writeAbs(filepath.Join(ext, "hooks", "hooks.json"), string(data)+"\n")
	sb.writeAbs(filepath.Join(ext, "gemini-extension.json"), `{"name":"terma","version":"1.0.0"}`+"\n")
}

func TestOpenCodeProfileEgress(t *testing.T) {
	forEachOpenCode(t, func(t *testing.T, b Binary, _ bool) {
		profileOtherOutOfBand(t, b, otherProfile{
			program:     "opencode",
			isModelCall: isChatCompletion,
			provider:    openAIChatProvider,
			setup: func(sb *Sandbox, url string) {
				sb.UseOpenCodeProvider(url)
				editJSON(t, filepath.Join(sb.Home, ".config", "opencode", "opencode.json"), func(doc map[string]any) {
					delete(doc, "autoupdate")
					delete(doc, "share")
				})
			},
			run: func(t *testing.T, sb *Sandbox, b Binary) []string {
				sb.OpenCodeRun(b, sb.Repo, "", profilePrompt)
				return []string{"run", "--format", "json", "--dir", "<repo>"}
			},
			exclude: []string{"<home>/.config/opencode/opencode.json"},
			kinds: []StorageKind{
				{regexp.MustCompile(`/storage/(session|message|part)/`), "transcript"},
				{regexp.MustCompile(`opencode[^/]*\.db`), "database"},
			},
		})
	})
}

func TestPiProfileEgress(t *testing.T) {
	forEachPi(t, func(t *testing.T, b Binary) {
		profileOtherOutOfBand(t, b, otherProfile{
			program:     "pi",
			isModelCall: isChatCompletion,
			provider:    openAIChatProvider,
			setup:       func(sb *Sandbox, url string) { sb.UsePiProvider(url) },
			run: func(t *testing.T, sb *Sandbox, b Binary) []string {
				args := []string{"-p", "--model", "fake/m"}
				runProfileCommand(t, b, sb.Repo, append(sb.termaEnv(), "PI_CODING_AGENT_DIR="+sb.piAgentDir()), append(args, profilePrompt)...)
				return args
			},
			exclude: []string{"<home>/.pi/agent/models.json"},
			kinds:   []StorageKind{{regexp.MustCompile(`/sessions/.*\.jsonl$`), "transcript"}},
		})
	})
}

func TestOmpProfileEgress(t *testing.T) {
	forEachOmp(t, func(t *testing.T, b Binary) {
		profileOtherOutOfBand(t, b, otherProfile{
			program:     "omp",
			isModelCall: isChatCompletion,
			provider:    openAIChatProvider,
			setup:       func(sb *Sandbox, url string) { sb.UseOmpProvider(url) },
			run: func(t *testing.T, sb *Sandbox, b Binary) []string {
				sb.OmpRun(b, sb.Repo, profilePrompt)
				return []string{"--model", "fake/m"}
			},
			exclude: []string{"<home>/.omp/agent/models.yml"},
			kinds:   []StorageKind{{regexp.MustCompile(`/sessions/.*\.jsonl$`), "transcript"}},
		})
	})
}

func TestHermesProfileEgress(t *testing.T) {
	forEachHermes(t, func(t *testing.T, b Binary) {
		profileOtherOutOfBand(t, b, otherProfile{
			program:     "hermes",
			isModelCall: isChatCompletion,
			provider:    func(calls *atomic.Int32) http.Handler { return hermesProvider(calls, "", nil) },
			setup:       func(sb *Sandbox, url string) { sb.UseHermes(url) },
			run: func(t *testing.T, sb *Sandbox, b Binary) []string {
				sb.HermesRun(b, sb.Repo, profilePrompt)
				return []string{"chat", "-Q", "--yolo"}
			},
			exclude: []string{"<home>/.hermes/config.yaml"},
			kinds:   []StorageKind{{regexp.MustCompile(`^<home>/\.hermes/sessions/`), "transcript"}},
		})
	})
}

func TestDshProfileEgress(t *testing.T) {
	forEachDsh(t, func(t *testing.T, b Binary) {
		profileOtherOutOfBand(t, b, otherProfile{
			program:     "dsh",
			isModelCall: isClaudeModelCall,
			provider:    func(calls *atomic.Int32) http.Handler { return claudeWorkloadProvider(calls, nil, false) },
			setup: func(sb *Sandbox, url string) {
				sb.UseDsh(url)
				// DeepSeek's own session telemetry is on by default.
				sb.ExtraEnv = slices.DeleteFunc(sb.ExtraEnv, func(kv string) bool { return strings.HasPrefix(kv, "DSH_TELEMETRY_MODE=") })
			},
			run: func(t *testing.T, sb *Sandbox, b Binary) []string {
				sb.DshRun(b, sb.Repo, profilePrompt)
				return []string{"headless"}
			},
			kinds: []StorageKind{{regexp.MustCompile(`/sessions/session-<uuid>\.json$`), "transcript"}},
		})
	})
}

// TestCopilotProfileBinary records each Copilot CLI build's executable, its signing and its
// help, with no turn: as TestClaudeProfileBinary, for a backfill of many builds' facts.
func TestCopilotProfileBinary(t *testing.T) {
	if !Enabled() {
		t.Skip("live tests run only with TERMA_E2E=1")
	}
	for _, b := range CopilotBinaries(t) {
		t.Run(b.Label(), func(t *testing.T) {
			scenarioOf(t, b.Harness)
			track(t)
			home := t.TempDir()
			RecordBinaryFacts(t, b)
			RecordSigning(t, b)
			RecordHelpTree(t, b, []string{"HOME=" + home, "COPILOT_HOME=" + filepath.Join(home, ".copilot"), "PATH=" + os.Getenv("PATH")}, "copilot")
			Placeholder(t, b, home, "<home>")
		})
	}
}

// TestGeminiProfileTelemetry records how each Gemini CLI build exports its own telemetry, with
// the exporter the field census's direct runs use (UseGeminiDirect), as
// TestClaudeProfileTelemetry does for Claude Code.
func TestGeminiProfileTelemetry(t *testing.T) {
	forEachGemini(t, func(t *testing.T, b Binary) {
		track(t)
		sb := New(t, Isolated)
		front := TimeReceiver(t, sb.Receiver)
		var hold HoldFirst
		var calls atomic.Int32
		isModelCall := func(r *http.Request) bool { return strings.Contains(r.URL.Path, "enerateContent") }
		provider := httptest.NewServer(hold.Wrap(telemetryHold, isModelCall, geminiProvider(&calls, "", nil)))
		defer provider.Close()
		sb.UseGemini(provider.URL)
		sb.UseGeminiDirect()
		repointExporter(t, filepath.Join(sb.Home, ".gemini", "settings.json"), sb.Receiver.URL(), front.URL)
		PlaceSandbox(t, b, sb)
		Placeholder(t, b, provider.URL, "<provider>")
		Placeholder(t, b, front.URL, "<receiver>")
		args := []string{"--output-format", "json"}
		RecordCommand(t, b, args)
		runProfileCommand(t, b, sb.Repo, sb.termaEnv(), append([]string{"-p", profilePrompt}, args...)...)
		time.Sleep(egressSettle)
		RecordTelemetry(t, b, front, hold.Answered())
	})
}
