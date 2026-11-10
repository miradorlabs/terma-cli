package e2e

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Muse Code (Meta) runs terma's hooks from its user-level settings.json
// (internal/agents/muse), and has no exporter to point anywhere. Builds come from Meta's
// download host by their full build string (1.4.15-R8339.1): a channel names its current build,
// and a build's manifest its binary for each platform with the binary's checksum. Nothing lists
// the builds before the current one, so `lastN` is the current build alone. Its Meta provider
// speaks the OpenAI Responses API at --base-url, so the scenarios run against a local fixture,
// with a synthetic key and no account; --model names the model, which spares the fixture
// Muse's model catalog.

const (
	museChannelURL  = "https://api.meta.ai/muse-code/channels/muse-stable"
	museDownloadURL = "https://lookaside.facebook.com/lookaside/muse/download/"
	// museModel is the model the scenarios ask for: Muse's default.
	museModel = "muse-wonder-1.0"
)

// MuseBinaries resolves the Muse builds to test (TERMA_E2E_MUSE_VERSIONS, the same spec as
// the others'): by default the installed one and the channel's current build.
func MuseBinaries(t *testing.T) []Binary {
	t.Helper()
	return resolve(t, "muse", cmp.Or(os.Getenv("TERMA_E2E_MUSE_VERSIONS"), "installed,last1"), museLatest, ensureMuse)
}

// museLatest is the stable channel's current build: the one build Meta names.
func museLatest() ([]string, error) {
	data, err := fetch(museChannelURL)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if err := CheckPathName("muse version", doc.Version); err != nil {
		return nil, err
	}
	return []string{doc.Version}, nil
}

// musePlatform is the manifest's name for this machine's build.
func musePlatform() string {
	arch := map[string]string{"arm64": "aarch64", "amd64": "x86"}[runtime.GOARCH]
	return arch + "_" + map[string]string{"darwin": "macos", "linux": "linux"}[runtime.GOOS]
}

// museFileURL is the download host's address of one file of a build.
func museFileURL(version, file string) string {
	return museDownloadURL + "?" + url.Values{"channel": {"muse"}, "version": {version}, "file": {file}}.Encode()
}

// ensureMuse downloads one Muse build under the versions directory, checked against its
// manifest's checksum.
func ensureMuse(version string) (string, error) {
	if err := CheckPathName("muse version", version); err != nil {
		return "", err
	}
	path := filepath.Join(versionsDir(), "muse", version, "muse")
	if _, err := os.Stat(path); err == nil {
		return abs(path), nil
	}
	data, err := fetch(museFileURL(version, "manifest.json"))
	if err != nil {
		return "", err
	}
	var m struct {
		Version   string `json:"version"`
		Algorithm string `json:"checksum_algorithm"`
		Artifacts map[string]struct {
			URL      string `json:"url"`
			Checksum string `json:"checksum"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return "", fmt.Errorf("muse %s manifest: %w", version, err)
	}
	a, ok := m.Artifacts[musePlatform()]
	if m.Version != version || m.Algorithm != "sha256" || !ok || len(a.Checksum) != 64 {
		return "", fmt.Errorf("muse %s: no %s build in its manifest", version, musePlatform())
	}
	if !strings.HasPrefix(a.URL, museDownloadURL) {
		return "", fmt.Errorf("muse %s: its manifest names a build off the download host: %s", version, a.URL)
	}
	bin, err := fetch(a.URL)
	if err != nil {
		return "", err
	}
	if sum := sha256.Sum256(bin); hex.EncodeToString(sum[:]) != a.Checksum {
		return "", fmt.Errorf("muse %s: checksum mismatch", version)
	}
	if err := writeBuild(path, bin); err != nil {
		return "", err
	}
	return abs(path), nil
}

func forEachMuse(t *testing.T, run func(t *testing.T, b Binary)) {
	t.Helper()
	if !Enabled() {
		t.Skip("live tests run only with TERMA_E2E=1")
	}
	builds := MuseBinaries(t)
	if len(builds) == 0 {
		t.Skip("no Muse build to test")
	}
	for _, b := range builds {
		t.Run(b.Label(), func(t *testing.T) { scenarioOf(t, b.Harness); run(t, b) })
	}
}

// UseMuse points the sandbox's Muse at the fixture at url, with a synthetic key, and keeps the
// installed launcher from updating itself.
func (sb *Sandbox) UseMuse(url string) {
	sb.MuseBaseURL = url
	sb.ExtraEnv = append(sb.ExtraEnv, "META_API_KEY=synthetic", "MUSE_NO_AUTO_UPDATE=1")
}

// museArgs is one headless turn against the sandbox's fixture, prompt last: tools run without
// asking (--yolo, as a headless run that is to use them must).
func (sb *Sandbox) museArgs(prompt string, tools bool) []string {
	a := []string{"exec", "--model", museModel, "--base-url", sb.MuseBaseURL}
	if tools {
		a = append(a, "--yolo")
	}
	return append(a, prompt)
}

// UseMuseSetup makes b the `muse` on the sandbox's PATH, which terma's hooks start `muse
// serve` from, and sets the machine up again with Muse chosen.
func (sb *Sandbox) UseMuseSetup(b Binary, url string) {
	t := sb.T
	t.Helper()
	if err := os.Symlink(b.Path, filepath.Join(strings.TrimSuffix(sb.binDir(), ":"), "muse")); err != nil {
		t.Fatal(err)
	}
	sb.UseMuse(url)
	sb.SetupAgents = append(sb.SetupAgents, "muse")
	sb.Setup(nil, "--team", sb.ProjectID, "--harness", sb.setupList())
	sb.Receiver.Reset()
}

// MuseRun runs one headless turn of the `muse` on the sandbox's PATH in dir, tools allowed, and
// returns what it printed.
func (sb *Sandbox) MuseRun(dir, prompt string) string {
	t := sb.T
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(strings.TrimSuffix(sb.binDir(), ":"), "muse"), sb.museArgs(prompt, true)...)
	cmd.Dir = dir
	cmd.Env = sb.termaEnv()
	cmd.Stdin = nil
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("muse exec: %v\n%s", err, tail(out.String(), 3000))
	}
	return out.String()
}

// museCall is one tool call the fixture asks Muse's main thread for, by the tool's id in Muse's
// namespace ("muse.bash").
type museCall struct {
	Tool string
	Args map[string]any
}

// museBash is a call of Muse's shell tool.
func museBash(cmd string) museCall {
	return museCall{"muse.bash", map[string]any{"command": cmd}}
}

// museNoReminder is a reminder agent's decision to remind of nothing, every field its tool's
// strict schema requires.
var museNoReminder = map[string]any{"decision": "none", "reason": "nothing to remind", "advisory_text": nil, "confidence": nil,
	"priority": nil, "skill_id": nil, "visible_for_steps": nil}

// museProvider is Muse's Meta provider: an empty model catalog, and the Responses API, which
// answers the main thread's request n with steps[n-1], then replies TERMA_TELEMETRY_REPLY. A
// reminder agent Muse runs beside the main thread (X-Tbh-Agent-Role reminder:*) decides to
// remind of nothing, then replies.
func museProvider(calls *atomic.Int32, steps []museCall) http.Handler {
	var mu sync.Mutex
	threads := map[string]int{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/muse-code/models"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":[]}`)
			return
		case r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/responses"):
			http.NotFound(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		n := calls.Add(1)
		mu.Lock()
		threads[r.Header.Get("X-Tbh-Thread-Id")]++
		step := threads[r.Header.Get("X-Tbh-Thread-Id")]
		mu.Unlock()
		var call *museCall
		switch role := r.Header.Get("X-Tbh-Agent-Role"); {
		case strings.HasPrefix(role, "reminder:") && step == 1:
			call = &museCall{"muse.submit_reminder_decision", museNoReminder}
		case role == "main" && step <= len(steps):
			call = &steps[step-1]
		}
		writeMuseResponse(w, n, call)
	})
}

// writeMuseResponse streams one Responses API response as Muse's provider reads it, every
// event numbered and a reply's text in its content part's events: call where it is not nil, else
// the reply TERMA_TELEMETRY_REPLY.
func writeMuseResponse(w http.ResponseWriter, n int32, call *museCall) {
	w.Header().Set("Content-Type", "text/event-stream")
	seq := 0
	emit := func(name string, payload map[string]any) {
		payload["type"], payload["sequence_number"] = name, seq
		seq++
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
	}
	response := func(status string, output []any) map[string]any {
		return map[string]any{"id": fmt.Sprintf("resp_muse_%d", n), "object": "response", "created_at": 1, "model": museModel, "status": status, "output": output}
	}
	emit("response.created", map[string]any{"response": response("in_progress", []any{})})
	var item map[string]any
	if call != nil {
		id := fmt.Sprintf("fc_muse_%d", n)
		args, _ := json.Marshal(call.Args)
		item = map[string]any{"type": "function_call", "id": id, "call_id": fmt.Sprintf("call_muse_%d", n), "name": call.Tool, "arguments": string(args), "status": "completed"}
		emit("response.output_item.added", map[string]any{"output_index": 0, "item": merged(item, map[string]any{"arguments": "", "status": "in_progress"})})
		emit("response.function_call_arguments.delta", map[string]any{"item_id": id, "output_index": 0, "delta": string(args)})
		emit("response.function_call_arguments.done", map[string]any{"item_id": id, "output_index": 0, "arguments": string(args)})
	} else {
		id := fmt.Sprintf("msg_muse_%d", n)
		part := map[string]any{"type": "output_text", "text": "TERMA_TELEMETRY_REPLY", "annotations": []any{}}
		item = map[string]any{"type": "message", "id": id, "role": "assistant", "status": "completed", "content": []any{part}}
		at := map[string]any{"item_id": id, "output_index": 0, "content_index": 0}
		emit("response.output_item.added", map[string]any{"output_index": 0, "item": merged(item, map[string]any{"status": "in_progress", "content": []any{}})})
		emit("response.content_part.added", merged(at, map[string]any{"part": merged(part, map[string]any{"text": ""})}))
		emit("response.output_text.delta", merged(at, map[string]any{"delta": "TERMA_TELEMETRY_REPLY"}))
		emit("response.output_text.done", merged(at, map[string]any{"text": "TERMA_TELEMETRY_REPLY"}))
		emit("response.content_part.done", merged(at, map[string]any{"part": part}))
	}
	emit("response.output_item.done", map[string]any{"output_index": 0, "item": item})
	done := response("completed", []any{item})
	done["usage"] = map[string]any{"total_tokens": 16, "input_tokens": 12, "output_tokens": 4,
		"input_tokens_details": map[string]any{"cached_tokens": 3}, "output_tokens_details": map[string]any{"reasoning_tokens": 1}}
	emit("response.completed", map[string]any{"response": done})
}

// merged is a copy of a with b's keys over it.
func merged(a, b map[string]any) map[string]any {
	out := maps.Clone(a)
	maps.Copy(out, b)
	return out
}
