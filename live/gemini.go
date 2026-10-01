package live

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Gemini CLI (@google/gemini-cli) exports OTLP natively from its user settings file,
// and runs terma's user-level Gemini extension's hooks (internal/agents/gemini).
// Builds come from npm; the fake model speaks the Gemini API
// (models/{m}:streamGenerateContent?alt=sse) at GOOGLE_GEMINI_BASE_URL.

// GeminiBinaries resolves the Gemini CLI builds to test (TERMA_LIVE_GEMINI_VERSIONS,
// the same spec as the others').
func GeminiBinaries(t *testing.T) []Binary {
	t.Helper()
	return resolve(t, "gemini", os.Getenv("TERMA_LIVE_GEMINI_VERSIONS"), func() ([]string, error) { return npmReleases("@google/gemini-cli") }, ensureGemini)
}

func forEachGemini(t *testing.T, run func(t *testing.T, b Binary)) {
	t.Helper()
	if !Enabled() {
		t.Skip("live tests run only with TERMA_LIVE=1")
	}
	builds := GeminiBinaries(t)
	if len(builds) == 0 {
		t.Skip("no Gemini CLI build to test")
	}
	for _, b := range builds {
		t.Run(b.Label(), func(t *testing.T) { run(t, b) })
	}
}

// ensureGemini installs one Gemini CLI release under the versions directory.
func ensureGemini(version string) (string, error) {
	dir := filepath.Join(versionsDir(), "gemini", version)
	bin := filepath.Join(dir, "node_modules", ".bin", "gemini")
	if _, err := os.Stat(bin); err == nil {
		return abs(bin), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "npm", "install", "--silent", "--no-audit", "--no-fund", "--prefix", dir, "@google/gemini-cli@"+version)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("npm install @google/gemini-cli@%s: %v\n%s", version, err, out)
	}
	return abs(bin), nil
}

// UseGemini sets the sandbox's Gemini up against a fake Gemini API: its API-key login,
// no usage statistics or updates, and the repository trusted, as the developer would
// have it. GEMINI_CLI_HOME is the sandbox home, in every environment the sandbox builds.
func (sb *Sandbox) UseGemini(url string, trusted ...string) {
	sb.ExtraEnv = append(sb.ExtraEnv, "GEMINI_CLI_HOME="+sb.Home, "GEMINI_API_KEY=synthetic", "GOOGLE_GEMINI_BASE_URL="+url)
	settings := map[string]any{
		"security": map[string]any{"auth": map[string]any{"selectedType": "gemini-api-key"}},
		"privacy":  map[string]any{"usageStatisticsEnabled": false},
		"general":  map[string]any{"enableAutoUpdate": false, "enableAutoUpdateNotification": false},
	}
	data, _ := json.MarshalIndent(settings, "", "  ")
	sb.writeAbs(filepath.Join(sb.Home, ".gemini", "settings.json"), string(data)+"\n")
	folders := map[string]string{sb.Repo: "TRUST_FOLDER"}
	for _, d := range trusted {
		folders[d] = "TRUST_FOLDER"
	}
	data, _ = json.Marshal(folders)
	sb.writeAbs(filepath.Join(sb.Home, ".gemini", "trustedFolders.json"), string(data)+"\n")
}

// UseGeminiDirect points Gemini's exporter straight at the receiver and writes terma's
// extension, as the direct half of a comparison — what `terma relay setup --harness
// gemini` writes, with the receiver for the relay.
func (sb *Sandbox) UseGeminiDirect() {
	t := sb.T
	t.Helper()
	path := filepath.Join(sb.Home, ".gemini", "settings.json")
	var doc map[string]any
	data, _ := os.ReadFile(path)
	_ = json.Unmarshal(data, &doc)
	doc["telemetry"] = map[string]any{"enabled": true, "target": "local", "otlpEndpoint": sb.Receiver.URL(), "otlpProtocol": "http", "logPrompts": true, "traces": true}
	data, _ = json.MarshalIndent(doc, "", "  ")
	sb.writeAbs(path, string(data)+"\n")
	hooks := map[string]any{}
	for _, e := range [][2]string{{"SessionStart", "gemini-session-start"}, {"BeforeAgent", "gemini-prompt"}, {"AfterTool", "gemini-after-tool"}, {"SessionEnd", "gemini-session-end"}} {
		hooks[e[0]] = []any{map[string]any{"matcher": "*", "hooks": []any{map[string]any{"type": "command", "command": "'" + sb.Terma + "' 'hook' " + e[1]}}}}
	}
	data, _ = json.Marshal(map[string]any{"hooks": hooks})
	ext := filepath.Join(sb.Home, ".gemini", "extensions", "terma")
	sb.writeAbs(filepath.Join(ext, "hooks", "hooks.json"), string(data)+"\n")
	sb.writeAbs(filepath.Join(ext, "gemini-extension.json"), `{"name":"terma","version":"1.0.0"}`+"\n")
}

// GeminiRun runs one headless `gemini -p` in dir, tools approved, and returns the
// session id it reports.
func (sb *Sandbox) GeminiRun(b Binary, dir, prompt string) string {
	t := sb.T
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.Path, "-p", prompt, "--yolo", "-m", "gemini-2.5-flash", "--output-format", "json")
	cmd.Dir = dir
	cmd.Env = sb.termaEnv()
	cmd.Stdin = nil
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("gemini -p: %v\n%s\n%s", err, stdout.String(), tail(stderr.String(), 3000))
	}
	var res struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(stdout.Bytes(), &res)
	return res.SessionID
}

// geminiProvider is a Gemini API endpoint the way Gemini CLI uses one: routing and
// classifier requests (JSON-schema responses) get a JSON answer, the turn asks once for
// tool with args (none when tool is empty) and then replies, with usage metadata.
func geminiProvider(calls *atomic.Int32, tool string, args map[string]any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(r.URL.Path, ":countTokens") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"totalTokens":42}`)
			return
		}
		if !strings.Contains(r.URL.Path, "generateContent") && !strings.Contains(r.URL.Path, "GenerateContent") {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		var req struct {
			Contents []struct {
				Parts []map[string]any `json:"parts"`
			} `json:"contents"`
			GenerationConfig map[string]any `json:"generationConfig"`
		}
		_ = json.Unmarshal(body, &req)
		answered := false
		for _, c := range req.Contents {
			for _, p := range c.Parts {
				if _, ok := p["functionResponse"]; ok {
					answered = true
				}
			}
		}
		var parts []any
		switch {
		case req.GenerationConfig["responseSchema"] != nil || req.GenerationConfig["responseJsonSchema"] != nil || req.GenerationConfig["responseMimeType"] == "application/json":
			parts = []any{map[string]any{"text": `{"reasoning":"fake","model_choice":"flash","next_speaker":"user"}`}}
		case tool != "" && !answered:
			parts = []any{map[string]any{"functionCall": map[string]any{"name": tool, "args": args}}}
		default:
			parts = []any{map[string]any{"text": "TERMA_TELEMETRY_REPLY"}}
		}
		resp := map[string]any{
			"candidates":    []any{map[string]any{"content": map[string]any{"role": "model", "parts": parts}, "finishReason": "STOP", "index": 0}},
			"usageMetadata": map[string]any{"promptTokenCount": 100, "candidatesTokenCount": 7, "totalTokenCount": 107},
			"modelVersion":  "gemini-2.5-flash", "responseId": fmt.Sprintf("resp-%d", calls.Load()),
		}
		data, _ := json.Marshal(resp)
		if !strings.Contains(r.URL.Path, "stream") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(data)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\r\n\r\n", data)
	})
}
