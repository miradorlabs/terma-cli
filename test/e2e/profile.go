package e2e

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// The harness profile: what each harness build asks its model provider for on a user's first
// turn, with nothing of terma's or the suite's in the way (its own system prompt, tools, model
// and cache settings), and its command line help. A profile scenario (TestClaudeProfile,
// TestCodexProfile) runs one turn with the build's defaults against a provider wrapped in
// CaptureProvider and records the build's help (RecordHelp); TestMain writes the run's profiles
// to report/profiles.json, as it writes the field census beside it. They are the raw evidence a
// build's published snapshot is normalized from, and one build is compared against the build
// before it.
//
// The plain turn also records what it changes nothing to read: the files the build wrote
// (storage.go), its hooks' payloads and its executable (probes_oob.go, binaryfacts.go) and the
// help of each subcommand (clihelp.go). What would change how the build behaves is a scenario of
// its own, a profile row beside the plain turn's for the same build: its egress through a
// recording proxy (egress.go, Test…ProfileEgress), and its own telemetry export, timed
// (Test…ProfileTelemetry).

// ProviderRequest is one request a harness build sent its model provider, as it sent it, but
// for the values of headers that carry a credential.
type ProviderRequest struct {
	// Probe names what the scenario was finding out when the build sent it: "first-turn" for
	// a profile's plain turn, or a behaviour probe's name ("retries", "compaction", …).
	Probe  string              `json:"probe,omitempty"`
	Host   string              `json:"host,omitempty"` // egress through the recording proxy only
	Method string              `json:"method"`
	Path   string              `json:"path"`
	Query  string              `json:"query,omitempty"`
	Header map[string][]string `json:"header,omitempty"`
	// Body is a JSON body as sent (decompressed, where it was gzip); Text is any other body,
	// "base64:" and its bytes where they are not text.
	Body json.RawMessage `json:"body,omitempty"`
	Text string          `json:"text,omitempty"`
	// Status is what the fixture answered; At when the request arrived, so a probe can measure
	// a build's backoff between retries.
	Status int       `json:"status,omitempty"`
	At     time.Time `json:"at"`
	// When places an egress request against the run's model calls: "before" the first,
	// "during" them, or "after" the last had been answered.
	When string `json:"when,omitempty"`
	// Error is why the recording proxy could not read a request: a CONNECT whose TLS the
	// build refused (a pinned host) is kept with it, so the host is known to exist.
	Error string `json:"error,omitempty"`
}

// Profile is what one profile scenario saw of one harness build.
type Profile struct {
	Harness  string `json:"harness"`
	Version  string `json:"version"`
	Scenario string `json:"scenario"`
	Platform string `json:"platform"`
	// Failed marks a scenario that failed for the build: its requests may stop short of a
	// whole turn, or be an error path's.
	Failed bool `json:"failed,omitempty"`
	// Command is the scenario's command line, the prompt left out: what it set that a
	// user's own run would not (a fixture provider, a feature off) is said there.
	Command  []string          `json:"command,omitempty"`
	Requests []ProviderRequest `json:"requests"`
	// Help is each help command's output, by its arguments joined with spaces.
	Help map[string]string `json:"help,omitempty"`
	// Placeholders map a literal of this run (a sandbox path, the fixture's URL, the prompt)
	// to the name normalization puts in its place, so one build's snapshot reads the same
	// whichever machine and night took it.
	Placeholders map[string]string `json:"placeholders,omitempty"`
	// Probes are behaviour probes' findings, by probe name, each its own shape (retries,
	// compaction, caching across turns, subagents, tool output limits, model aliases, prices).
	Probes map[string]json.RawMessage `json:"probes,omitempty"`
	// Egress is every request the build made to anything but its model provider, through the
	// recording proxy: telemetry, feature flags, update checks.
	Egress []ProviderRequest `json:"egress,omitempty"`
	// Files are what the build wrote to disk, by kind ("transcript", "settings"): each its
	// path pattern and record shape.
	Files map[string]json.RawMessage `json:"files,omitempty"`
	// Binary is what the build's executable says of itself: runtime, bundled SDKs, size, and
	// the environment variables and settings keys it reads.
	Binary json.RawMessage `json:"binary,omitempty"`
	// Notes say how the capture differed from the harness as a user runs it, where the
	// scenario could not run it so (a desktop app driven without a person at it): they go into
	// the snapshot's capture notes.
	Notes []string  `json:"notes,omitempty"`
	At    time.Time `json:"at"`
}

// maxProfileBody bounds the body kept of one request; a harness's system prompt and tools are
// well under it.
const maxProfileBody = 16 << 20

// credentialHeaders are the headers whose values are never kept, lowercase.
var credentialHeaders = map[string]bool{
	"authorization": true, "proxy-authorization": true, "x-api-key": true, "x-goog-api-key": true,
	"api-key": true, "cookie": true, "chatgpt-account-id": true, "openai-organization": true,
	"openai-project": true,
}

type profileID struct{ harness, version, scenario string }

var (
	profilesMu sync.Mutex
	profiles   = map[profileID]*Profile{}
)

// profileOf is t's profile of b, made on first use; t's failure marks it failed.
func profileOf(t *testing.T, b Binary) *Profile {
	t.Helper()
	scenario, _, _ := strings.Cut(t.Name(), "/")
	id := profileID{b.Harness, b.Version, scenario}
	profilesMu.Lock()
	defer profilesMu.Unlock()
	if p := profiles[id]; p != nil {
		return p
	}
	p := &Profile{Harness: b.Harness, Version: b.Version, Scenario: scenario, Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Requests: []ProviderRequest{}, At: time.Now().UTC()}
	profiles[id] = p
	t.Cleanup(func() {
		profilesMu.Lock()
		defer profilesMu.Unlock()
		p.Failed = p.Failed || t.Failed()
	})
	return p
}

// CaptureProvider records every request harness build b sends next, in t's profile of b, as
// its first turn's, then hands it on unchanged.
func CaptureProvider(t *testing.T, b Binary, next http.Handler) http.Handler {
	t.Helper()
	return CaptureProbe(t, b, "first-turn", next)
}

// CaptureProbe is CaptureProvider for a behaviour probe: the requests are probe's.
func CaptureProbe(t *testing.T, b Binary, probe string, next http.Handler) http.Handler {
	t.Helper()
	p := profileOf(t, b)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(io.LimitReader(r.Body, maxProfileBody))
		if err != nil {
			t.Errorf("read the provider request: %v", err)
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		req := ProviderRequest{Probe: probe, At: time.Now().UTC(), Method: r.Method, Path: r.URL.Path, Query: RedactQuery(r.URL.RawQuery), Header: RedactHeader(r.Header)}
		req.Body, req.Text = profileBody(r.Header.Get("Content-Encoding"), data)
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		req.Status = sw.status
		profilesMu.Lock()
		p.Requests = append(p.Requests, req)
		profilesMu.Unlock()
	})
}

// statusWriter keeps the status a fixture answered.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Placeholder records that literal, wherever t's profile of b holds it, is name: "<repo>",
// "<home>", "<provider>", "<prompt>".
func Placeholder(t *testing.T, b Binary, literal, name string) {
	t.Helper()
	if literal == "" {
		return
	}
	p := profileOf(t, b)
	profilesMu.Lock()
	defer profilesMu.Unlock()
	if p.Placeholders == nil {
		p.Placeholders = map[string]string{}
	}
	p.Placeholders[literal] = name
}

// PlaceSandbox records the placeholders of sb's own paths.
func PlaceSandbox(t *testing.T, b Binary, sb *Sandbox) {
	t.Helper()
	for literal, name := range map[string]string{sb.Repo: "<repo>", sb.Home: "<home>", sb.Dir: "<sandbox>",
		sb.TermaConfig: "<terma-config>", sb.ClaudeConfig: "<claude-config>", sb.CodexHome: "<codex-home>", sb.WorkDir: "<workdir>"} {
		Placeholder(t, b, literal, name)
	}
	if root := npmPackageRoot(b.Path); root != "" {
		Placeholder(t, b, root, "<install>")
	}
}

// npmPackageRoot is the folder of the npm package that holds the executable at path (through
// its symlinks: node_modules/.bin/gemini is a link into the package), "" where it is none. A build
// tells its model where its own files are (Gemini CLI its built-in skills, Pi its docs), and that
// folder is where this machine installed it: a checkout's versions folder, a runner's tool cache,
// a home.
func npmPackageRoot(path string) string {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	for dir := filepath.Dir(real); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		parent := filepath.Dir(dir)
		if filepath.Base(parent) == "node_modules" ||
			strings.HasPrefix(filepath.Base(parent), "@") && filepath.Base(filepath.Dir(parent)) == "node_modules" {
			if fileExists(filepath.Join(dir, "package.json")) {
				return dir
			}
		}
	}
	return ""
}

// RecordProbe records what behaviour probe name found of b; v is marshalled as JSON.
func RecordProbe(t *testing.T, b Binary, name string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Errorf("probe %s: %v", name, err)
		return
	}
	p := profileOf(t, b)
	profilesMu.Lock()
	defer profilesMu.Unlock()
	if p.Probes == nil {
		p.Probes = map[string]json.RawMessage{}
	}
	p.Probes[name] = data
}

// RecordFile records the shape of a kind of file b wrote ("transcript", "settings").
func RecordFile(t *testing.T, b Binary, kind string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Errorf("file %s: %v", kind, err)
		return
	}
	p := profileOf(t, b)
	profilesMu.Lock()
	defer profilesMu.Unlock()
	if p.Files == nil {
		p.Files = map[string]json.RawMessage{}
	}
	p.Files[kind] = data
}

// RecordBinary records what b's executable says of itself.
func RecordBinary(t *testing.T, b Binary, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Errorf("binary facts: %v", err)
		return
	}
	p := profileOf(t, b)
	profilesMu.Lock()
	defer profilesMu.Unlock()
	p.Binary = data
}

// RecordNote records how t's capture of b differed from b as a user runs it.
func RecordNote(t *testing.T, b Binary, note string) {
	t.Helper()
	p := profileOf(t, b)
	profilesMu.Lock()
	defer profilesMu.Unlock()
	if !slices.Contains(p.Notes, note) {
		p.Notes = append(p.Notes, note)
	}
}

// RecordEgress records a request b made to anything but its model provider.
func RecordEgress(t *testing.T, b Binary, req ProviderRequest) {
	t.Helper()
	p := profileOf(t, b)
	profilesMu.Lock()
	defer profilesMu.Unlock()
	p.Egress = append(p.Egress, req)
}

// RedactHeader is h with lowercase names, and no credential's value.
func RedactHeader(h http.Header) map[string][]string {
	out := map[string][]string{}
	for name, values := range h {
		if credentialHeaders[strings.ToLower(name)] {
			values = []string{"[redacted]"}
		}
		out[strings.ToLower(name)] = values
	}
	return out
}

// RedactQuery is a raw query string with no credential's value: a parameter whose name says it
// holds a secret (key, token, signature, password, as a variable's does; sig), or whose value has
// a credential's shape, is "[redacted]", as a credential header's value is. The rest is as sent,
// in its order and encoding.
func RedactQuery(raw string) string {
	if raw == "" {
		return raw
	}
	parts := strings.Split(raw, "&")
	for i, part := range parts {
		k, v, ok := strings.Cut(part, "=")
		if !ok || v == "" {
			continue
		}
		name, err := url.QueryUnescape(k)
		if err != nil {
			name = k
		}
		value, err := url.QueryUnescape(v)
		if err != nil {
			value = v
		}
		if secretName(name) || strings.EqualFold(name, "sig") || secretValueRE.MatchString(value) {
			parts[i] = k + "=[redacted]"
		}
	}
	return strings.Join(parts, "&")
}

// ProfileBody is a request body as a profile keeps it: JSON as sent, or text.
func ProfileBody(encoding string, data []byte) (json.RawMessage, string) {
	return profileBody(encoding, data)
}

// profileBody is data as a JSON body or, where it is not JSON, as text.
func profileBody(encoding string, data []byte) (json.RawMessage, string) {
	if strings.EqualFold(encoding, "gzip") {
		if zr, err := gzip.NewReader(bytes.NewReader(data)); err == nil {
			if plain, err := io.ReadAll(io.LimitReader(zr, maxProfileBody)); err == nil {
				data = plain
			}
		}
	}
	switch {
	case len(data) == 0:
		return nil, ""
	case json.Valid(data):
		return json.RawMessage(data), ""
	case utf8.Valid(data):
		return nil, string(data)
	}
	return nil, "base64:" + base64.StdEncoding.EncodeToString(data)
}

// RecordHelp runs harness build b with args (its --help, a subcommand's) in env and records
// what it printed in t's profile of b. A build whose help fails is recorded with what it
// printed and the error, not failed: help is evidence, not a contract.
func RecordHelp(t *testing.T, b Binary, env []string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.Path, args...)
	cmd.Env = append(env, "NO_COLOR=1", "TERM=dumb")
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	text := string(out)
	if err != nil {
		t.Logf("%s %s: %v", b.Harness, strings.Join(args, " "), err)
		text += "\n[" + err.Error() + "]"
	}
	p := profileOf(t, b)
	profilesMu.Lock()
	defer profilesMu.Unlock()
	if p.Help == nil {
		p.Help = map[string]string{}
	}
	p.Help[strings.Join(args, " ")] = text
}

// RecordCommand records the command line t's profile of b ran, the prompt left out.
func RecordCommand(t *testing.T, b Binary, args []string) {
	t.Helper()
	p := profileOf(t, b)
	profilesMu.Lock()
	defer profilesMu.Unlock()
	p.Command = append([]string{b.Harness}, args...)
}

// WriteProfiles writes this run's profiles to dir/profiles.json, one a line. A run that took
// none writes none, and leaves no earlier run's behind.
func WriteProfiles(dir string) error {
	path := filepath.Join(dir, "profiles.json")
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	profilesMu.Lock()
	rows := make([]Profile, 0, len(profiles))
	for _, p := range profiles {
		rows = append(rows, *p)
	}
	profilesMu.Unlock()
	if len(rows) == 0 {
		return nil
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		return a.Harness+"\x00"+a.Version+"\x00"+a.Scenario < b.Harness+"\x00"+b.Version+"\x00"+b.Scenario
	})
	var buf bytes.Buffer
	buf.WriteString("[\n")
	for i, p := range rows {
		line, err := json.Marshal(p)
		if err != nil {
			return err
		}
		buf.Write(line)
		if i < len(rows)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	buf.WriteString("]\n")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}
