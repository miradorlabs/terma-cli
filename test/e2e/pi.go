package e2e

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Pi (@earendil-works/pi-coding-agent) has no OpenTelemetry of its own: terma's
// extension (internal/agents/pi/terma.ts), written into Pi's agent directory, is its
// exporter and calls `terma hook pi-*`. `terma setup --harness pi` writes it pointed at the
// relay (UsePi); a direct run splices the same template pointed at the receiver, so the two
// differ only in where the extension sends. The builds are the installed one, or
// TERMA_E2E_PI_VERSIONS's from npm: CI runs the floor (internal/agents/pi/minimum_version.go)
// and the latest.

// piPackage is Pi's npm package.
const piPackage = "@earendil-works/pi-coding-agent"

// piScope is the instrumentation scope of everything terma's extension exports.
const piScope = "terma-pi"

func forEachPi(t *testing.T, run func(t *testing.T, b Binary)) {
	t.Helper()
	if !Enabled() {
		t.Skip("live tests run only with TERMA_E2E=1")
	}
	skipUnlessSelected(t, "pi")
	spec := cmp.Or(os.Getenv("TERMA_E2E_PI_VERSIONS"), "installed")
	builds := resolve(t, "pi", spec, func() ([]string, error) { return npmReleases(piPackage) }, ensureNpmBuild("pi", piPackage, "pi"))
	if len(builds) == 0 {
		Record(t.Name(), "not run", "pi is not installed")
		t.Skip("pi is not installed")
	}
	for _, b := range builds {
		t.Run(b.Label(), func(t *testing.T) { scenarioOfBuild(t, b); run(t, b) })
	}
}

// piAgentDir is the sandbox's Pi configuration directory (PI_CODING_AGENT_DIR's
// default under the sandbox home).
func (sb *Sandbox) piAgentDir() string { return filepath.Join(sb.Home, ".pi", "agent") }

// UsePiProvider points the sandbox's Pi at an OpenAI-compatible endpoint, as its only
// model.
func (sb *Sandbox) UsePiProvider(url string) {
	cfg := map[string]any{"providers": map[string]any{"fake": map[string]any{
		"baseUrl": url + "/v1", "api": "openai-completions", "apiKey": "synthetic",
		"models": []any{map[string]any{"id": "m"}},
	}}}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	sb.writeAbs(filepath.Join(sb.piAgentDir(), "models.json"), string(data)+"\n")
}

// UsePiScript answers Pi's requests from each conversation's steps, picked by a key its
// prompt holds (openCodeConversationProvider: the same OpenAI-compatible script). A failFirst
// answers the first that many requests 500, which Pi retries.
func (sb *Sandbox) UsePiScript(failFirst int, convs ...openCodeConversation) {
	scripted := openCodeConversationProvider(convs)
	var failed atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chat/completions") && int(failed.Add(1)) <= failFirst {
			http.Error(w, `{"error":{"message":"scripted outage TERMA_PI_ERROR"}}`, http.StatusInternalServerError)
			return
		}
		scripted.ServeHTTP(w, r)
	}))
	sb.T.Cleanup(provider.Close)
	sb.UsePiProvider(provider.URL)
}

// piCall is a scripted reply asking for one call of Pi's tool name with args.
func piCall(id, name string, args map[string]any) openCodeStep {
	return openCodeStep{calls: []openCodeCall{{id: id, name: name, args: args}}}
}

// UsePi makes b the `pi` on the sandbox's PATH and sets the machine up again with Pi chosen,
// as a developer would: `terma setup` writes the extension and records Pi in the profile.
// It returns setup's output.
func (sb *Sandbox) UsePi(b Binary) string {
	t := sb.T
	t.Helper()
	link := filepath.Join(strings.TrimSuffix(sb.binDir(), ":"), "pi")
	if _, err := os.Lstat(link); err != nil {
		if err := os.Symlink(b.Path, link); err != nil {
			t.Fatal(err)
		}
	}
	sb.SetupAgents = append(sb.SetupAgents, "pi")
	sb.RelayAgents = append(sb.RelayAgents, "pi")
	out := sb.Setup(nil, "--team", sb.ProjectID, "--harness", sb.setupList())
	if !slices.Contains(sb.recordedHarnesses(), "pi") {
		t.Fatalf("terma setup --harness %s did not record Pi: %v\n%s", sb.setupList(), sb.recordedHarnesses(), out)
	}
	sb.Receiver.Reset()
	return out
}

// piRelayIn is sb with Pi set up through terma setup, answered by convs, and the relay
// running under the policy o.
func piRelayIn(sb *Sandbox, b Binary, o RelayOptions, convs ...openCodeConversation) *Sandbox {
	sb.UsePiScript(0, convs...)
	sb.UsePi(b)
	o.Start = true
	sb.UseRelay(o)
	return sb
}

// UsePiExtensionDirect writes terma's Pi extension pointed straight at the receiver,
// as the direct half of a comparison.
func (sb *Sandbox) UsePiExtensionDirect() {
	sb.T.Helper()
	sb.writePiExtension(filepath.Join(sb.piAgentDir(), "extensions", "terma.ts"))
}

// writePiExtension splices Pi's extension template pointed at the receiver into path.
func (sb *Sandbox) writePiExtension(path string) {
	t := sb.T
	t.Helper()
	tmpl, err := os.ReadFile(filepath.Join("..", "..", "internal", "agents", "pi", "terma.ts"))
	if err != nil {
		t.Fatal(err)
	}
	const marker = "const CONFIG: TermaConfig | null = null /* terma:config */"
	cfg, _ := json.Marshal(map[string]any{
		"version": 1, "endpoint": sb.Receiver.URL(), "headers": map[string]string{"Authorization": "Bearer " + liveKey},
		"includePrompts": true, "includeToolContent": true, "hookCommand": []string{sb.Terma, "hook"},
	})
	if !bytes.Contains(tmpl, []byte(marker)) {
		t.Fatal("the Pi extension template has no configuration line")
	}
	sb.writeAbs(path, strings.Replace(string(tmpl), marker, "const CONFIG: TermaConfig | null = "+string(cfg)+" /* terma:config */", 1))
}

// PiRun runs one `pi -p` in dir (the live tests run from test/e2e/, next to the template
// UsePiExtensionDirect reads), extra flags before the prompt, and returns its output.
func (sb *Sandbox) PiRun(b Binary, dir, prompt string, flags ...string) string {
	t := sb.T
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	args := append([]string{"-p", "--model", "fake/m", "--offline", "--no-context-files", "--no-skills"}, flags...)
	cmd := exec.CommandContext(ctx, b.Path, append(args, prompt)...)
	cmd.Dir = dir
	cmd.Env = append(sb.termaEnv(), "PI_CODING_AGENT_DIR="+sb.piAgentDir())
	cmd.Stdin = nil
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("pi -p: %v\n%s", err, out.String())
	}
	return out.String()
}

// PiSession runs one `pi -p` as PiRun does and returns the session its start hook announced.
func (sb *Sandbox) PiSession(b Binary, dir, prompt string, flags ...string) string {
	t := sb.T
	t.Helper()
	before := len(sb.Events("terma.session.start", ""))
	sb.PiRun(b, dir, prompt, flags...)
	starts := sb.WaitEvents("terma.session.start", "", 10*time.Second)
	if len(starts) <= before {
		t.Fatalf("Pi announced no session for %q; spool: %+v", prompt, sb.Spool())
	}
	return starts[len(starts)-1].SessionID
}
