package omp

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// exporter configures omp's export to Terma by installing a hook extension.
//
// omp has a native OTLP exporter — spans per model call carrying tokens, effort,
// service tier and latency — but it is configured exclusively through OTEL_* process
// env; its YAML settings file cannot set them, and there is no project-scope env
// mechanism. What omp does offer is a user hooks directory (~/.omp/agent/hooks/pre)
// whose TypeScript files are loaded at startup, before initTelemetryExport reads the
// env. So the harness is an extension: a single dependency-free TypeScript file,
// embedded in this binary and written to that directory with a config line spliced in.
// The extension exports the OTEL_* variables into process.env, hands session lifecycle
// and file edits to `terma hook omp-*` for attribution, and posts the one figure the
// native exporter cannot compute — estimated cost — as a companion log record.
//
// The key never sits in the file: like Claude Code's headers helper, the extension
// runs Terma's helper script for its Authorization header. ~/.omp/agent/config.yml is
// never touched.
//
// Repository scope is a policy file, <root>/.omp/terma.json, that the extension lays
// over its global settings: which signals ship and whether prompt and tool content go
// with them. Nothing about where or with which key, so it is safe to commit.
type exporter struct {
	// root, when set, is the repository whose policy file this value acts on.
	root string
}

//go:embed extension/terma.ts
var ompExtensionSource string

const (
	ompServiceName    = "omp"
	ompHooksDir       = "hooks/pre"
	ompExtensionFile  = "terma.ts"
	ompLocalPolicy    = ".omp/terma.json"
	ompConfigOverride = "OMP_DIR"

	// ompConfigPlaceholder is the one line of the embedded extension that a connect
	// replaces. The raw file ships with CONFIG null, which makes the extension inert,
	// so a template that somehow reached omp unspliced does nothing rather than
	// something wrong.
	ompConfigPlaceholder = "const CONFIG = null /* terma:config */"
	ompConfigPrefix      = "const CONFIG = "

	// The upstream switch for prompt/response content capture.
	ompCaptureContentEnv = "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT"
)

// Name is the token `terma connect` and `--harness` accept.
func (exporter) Name() string { return "omp" }

// DisplayName is how the agent is written in prose.
func (exporter) DisplayName() string { return "Omp" }

// SupportsHeadersHelper is true: the extension runs the helper script itself.
func (exporter) SupportsHeadersHelper() bool { return true }

// Local returns the harness bound to the repository at root.
func (exporter) Local(root string) harness.Harness { return exporter{root: root} }

// Scope reports which layer this value acts on.
func (c exporter) Scope() harness.Scope {
	if c.root != "" {
		return harness.ScopeLocal
	}
	return harness.ScopeGlobal
}

// Detect runs `omp --version`. A missing binary is not-found rather than an error.
func (exporter) Detect(ctx context.Context) harness.Detection {
	return harness.DetectBinary(ctx, "omp", harness.SemverRE)
}

// ompAgentDir is where omp keeps its user configuration and hooks: $OMP_DIR/agent,
// else ~/.omp/agent, on every platform.
func ompAgentDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv(ompConfigOverride)); dir != "" {
		return filepath.Join(dir, "agent"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".omp", "agent"), nil
}

// ConfigPath is the extension file omp loads, or the repository's policy file.
func (c exporter) ConfigPath() (string, error) {
	if c.root != "" {
		return filepath.Join(c.root, filepath.FromSlash(ompLocalPolicy)), nil
	}
	dir, err := ompAgentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.FromSlash(ompHooksDir), ompExtensionFile), nil
}

// ompConfig is the user-scope file: everything a session needs to reach Terma, spliced
// into the extension source.
type ompConfig struct {
	Version            int               `json:"version"`
	Endpoint           string            `json:"endpoint,omitempty"`
	Headers            map[string]string `json:"headers,omitempty"`
	HeadersHelper      string            `json:"headersHelper,omitempty"`
	Signals            []string          `json:"signals"`
	IncludePrompts     bool              `json:"includePrompts"`
	IncludeToolContent bool              `json:"includeToolContent"`
	ResourceAttributes map[string]string `json:"resourceAttributes,omitempty"`

	// HookCommand is how the extension reaches `terma hook` for commit attribution.
	HookCommand []string `json:"hookCommand,omitempty"`

	// PerRepo switches the extension from one fixed destination to resolving the
	// repository's own binding at runtime. In that mode Headers and HeadersHelper are
	// empty and the project's helper is looked up per session.
	PerRepo          bool   `json:"perRepo,omitempty"`
	HelpersDir       string `json:"helpersDir,omitempty"`
	HelperPrefix     string `json:"helperPrefix,omitempty"`
	ProjectAttribute string `json:"projectAttribute,omitempty"`
}

// ompPolicy is the repository-scope file: the subset of the config a repository may
// decide.
type ompPolicy struct {
	Signals            []string `json:"signals"`
	IncludePrompts     bool     `json:"includePrompts"`
	IncludeToolContent bool     `json:"includeToolContent"`
}

// config builds what the extension will read. At repository scope only the policy
// fields are kept; the destination and credential are the global connect's.
func (c exporter) config(e harness.Exporter) ompConfig {
	cfg := ompConfig{
		Version:            1,
		Signals:            harness.SignalNames(e.Signals),
		IncludePrompts:     e.IncludePrompts,
		IncludeToolContent: e.IncludeToolContent,
	}
	if c.root != "" {
		return cfg
	}
	cfg.Endpoint = e.Endpoint
	switch {
	case e.HelperPath != "":
		cfg.HeadersHelper = e.HelperPath
	case e.APIKey != "":
		cfg.Headers = map[string]string{"Authorization": "Bearer " + e.APIKey}
	}
	if len(e.ResourceAttributes) > 0 {
		cfg.ResourceAttributes = map[string]string{}
		for k, v := range e.ResourceAttributes {
			if k != "" && v != "" {
				cfg.ResourceAttributes[k] = v
			}
		}
	}
	cfg.HookCommand = []string{"terma", "hook"}
	return cfg
}

// renderExtension splices the config into the embedded extension source.
func renderOmpExtension(cfg ompConfig) ([]byte, error) {
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	if strings.Count(ompExtensionSource, ompConfigPlaceholder) != 1 {
		return nil, fmt.Errorf("omp extension source must contain exactly one %q", ompConfigPlaceholder)
	}
	return []byte(strings.Replace(ompExtensionSource, ompConfigPlaceholder, ompConfigPrefix+string(encoded), 1)), nil
}

// readOmpExtensionConfig extracts the spliced config line back out of an installed
// extension file. A file without it is one Terma did not write.
func readOmpExtensionConfig(data []byte) (*ompConfig, bool) {
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, ompConfigPrefix) || strings.Contains(line, ompConfigPlaceholder) {
			continue
		}
		var cfg ompConfig
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, ompConfigPrefix)), &cfg); err != nil {
			return nil, false
		}
		return &cfg, true
	}
	return nil, false
}

// Status reads the installed extension back.
func (c exporter) Status() (harness.Status, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return harness.Status{}, err
	}
	status := harness.Status{ConfigPath: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return status, nil
	}
	if err != nil {
		return harness.Status{}, fmt.Errorf("read %s: %w", path, err)
	}
	status.Exists = true

	if c.root != "" {
		var policy ompPolicy
		if json.Unmarshal(data, &policy) != nil {
			return status, nil
		}
		status.HasPolicy = true
		status.Signals = harness.SignalsFromNames(policy.Signals)
		status.IncludePrompts = policy.IncludePrompts
		status.IncludeToolContent = policy.IncludeToolContent
		status.ManagedKeys = 1
		return status, nil
	}

	cfg, ok := readOmpExtensionConfig(data)
	if !ok {
		return status, nil
	}
	status.ManagedKeys = 1
	status.Endpoint = cfg.Endpoint
	status.Signals = harness.SignalsFromNames(cfg.Signals)
	status.IncludePrompts = cfg.IncludePrompts
	status.IncludeToolContent = cfg.IncludeToolContent
	status.ProjectID = cfg.ResourceAttributes[harness.AttrProjectID]
	// Connected means a destination and a way to authenticate to it.
	status.Connected = cfg.Endpoint != "" && (cfg.HeadersHelper != "" || cfg.Headers["Authorization"] != "")
	status.KeyPrefix = harness.MaskKey(ompKey(cfg))
	status.Conflicts = ompConflicts(harness.Exporter{Endpoint: cfg.Endpoint})
	return status, nil
}

// ompKey is the raw key a config presents, from the helper or inline.
func ompKey(cfg *ompConfig) string {
	if cfg.HeadersHelper != "" && harness.IsOwnHelper(cfg.HeadersHelper) {
		if key := harness.KeyFromHelper(cfg.HeadersHelper); key != "" {
			return key
		}
	}
	if v := cfg.Headers["Authorization"]; v != "" {
		return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "Bearer"))
	}
	return ""
}

// ConflictsWith reports what would defeat or redirect the export e describes.
func (c exporter) ConflictsWith(e harness.Exporter) ([]harness.Conflict, error) {
	return ompConflicts(e), nil
}

// ompConflicts reports shell-exported OTEL_* variables that outrank what the extension
// sets. The extension deliberately never overrides a variable the shell already
// exported — a developer who pointed OTEL_EXPORTER_OTLP_ENDPOINT somewhere did so on
// purpose — so an export that disagrees with the connect is reported rather than
// fought.
func ompConflicts(e harness.Exporter) []harness.Conflict {
	var out []harness.Conflict

	// The generic endpoint pointing at another collector takes effect over the
	// extension's set: process.env was already populated by the shell.
	if value := os.Getenv(harness.EnvOTLPEndpoint); value != "" && value != e.Endpoint {
		out = append(out, harness.Conflict{
			Key:        harness.EnvOTLPEndpoint,
			Value:      value,
			Reason:     "exported in your shell, where it takes effect regardless of what Terma's extension sets — the export goes there, not to Terma",
			Credential: true,
			Scope:      harness.ScopeEnvironment,
			Clearable:  false,
		})
	}

	// A protocol mismatch silently disables the export: omp supports http/protobuf
	// only. The extension pins the variable, but a shell export is still the user's to
	// unset.
	if value := strings.ToLower(strings.TrimSpace(os.Getenv(harness.EnvOTLPProtocol))); value != "" && value != harness.ProtocolHTTPProtobuf {
		out = append(out, harness.Conflict{
			Key:       harness.EnvOTLPProtocol,
			Value:     value,
			Reason:    "omp's exporter supports http/protobuf only; a " + value + " export would disable every signal",
			Scope:     harness.ScopeEnvironment,
			Clearable: false,
		})
	}

	// The upstream content switch exported in the shell outranks the policy the
	// connect wrote into the extension.
	if value := os.Getenv(ompCaptureContentEnv); value != "" && !matchesCapture(value, e) {
		out = append(out, harness.Conflict{
			Key:       ompCaptureContentEnv,
			Value:     value,
			Reason:    "exported in your shell, where it decides content capture instead of the repository's policy",
			Scope:     harness.ScopeEnvironment,
			Clearable: false,
			Advisory:  true,
		})
	}
	return out
}

// matchesCapture reports whether an exported capture value agrees with the exporter's
// content posture, so a shell that happens to set the same value is not a conflict.
func matchesCapture(value string, e harness.Exporter) bool {
	want := e.IncludePrompts || e.IncludeToolContent
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "summary":
		return want
	case "false", "0", "":
		return !want
	}
	return false
}

// Connect writes the extension (and its helper script), or the repository's policy
// file. The file is generated whole, so there is nothing to merge and nothing to
// journal: disconnect removes exactly this file.
func (c exporter) Connect(e harness.Exporter, _ bool) error {
	path, err := c.ConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	cfg := c.config(e)

	if c.root != "" {
		data, err := json.MarshalIndent(ompPolicy{
			Signals: cfg.Signals, IncludePrompts: cfg.IncludePrompts, IncludeToolContent: cfg.IncludeToolContent,
		}, "", "  ")
		if err != nil {
			return err
		}
		return config.WriteFileAtomic(path, append(data, '\n'), 0o644)
	}

	// The helper first: the extension about to be written points at it, and an omp
	// starting between the two writes must find the credential already there.
	if e.HelperPath != "" {
		if err := harness.WriteHelper(e.HelperPath, e.APIKey); err != nil {
			return err
		}
	}
	src, err := renderOmpExtension(cfg)
	if err != nil {
		return err
	}
	// 0600 only when the key is inline; with the helper the file holds a path, not a
	// secret, and stays readable like the user's other hooks.
	mode := fs.FileMode(0o644)
	if len(cfg.Headers) > 0 {
		mode = harness.SettingsMode
	}
	return config.WriteFileAtomic(path, src, mode)
}

// ConnectPerRepo sets up omp to export per repository: it writes this project's
// headers helper (holding the key), and installs the global extension in per-repo mode
// so that a session in any bound repository reports to that repository's project.
//
// The extension file is global and shared across projects; re-running this for another
// project rewrites it (idempotently) and adds that project's helper. The extension
// carries no key and no fixed project — only the endpoint, the helpers directory and
// the naming convention it resolves a project's helper with at runtime.
func (exporter) ConnectPerRepo(e harness.Exporter) error {
	helper, err := harness.HelperFilePath(exporter{}, e.ProjectID)
	if err != nil {
		return err
	}
	if err := harness.WriteHelper(helper, e.APIKey); err != nil {
		return err
	}
	helpersDir, err := harness.HelpersDir()
	if err != nil {
		return err
	}
	cfg := ompConfig{
		Version:  1,
		Endpoint: e.Endpoint,
		Signals:  harness.SignalNames(e.Signals),
		// The extension file is global and shared across every bound repository, so
		// this repo's content-capture choice must NOT ride in it — otherwise installing
		// one project would flip prompt / tool-content capture on for every other
		// project that has no policy of its own. Content capture is off in the shared
		// floor and is opt-in per repository through its committed .omp/terma.json
		// overlay (which the extension lays over these defaults for that repository
		// only).
		IncludePrompts:     false,
		IncludeToolContent: false,
		ResourceAttributes: ompBaseAttributes(e),
		HookCommand:        []string{"terma", "hook"},
		PerRepo:            true,
		HelpersDir:         helpersDir,
		HelperPrefix:       exporter{}.Name() + "-otel-",
		ProjectAttribute:   harness.AttrProjectID,
	}
	path, err := (exporter{}).ConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	src, err := renderOmpExtension(cfg)
	if err != nil {
		return err
	}
	// The key lives in the helper, not the extension, so this file stays readable.
	return config.WriteFileAtomic(path, src, 0o644)
}

// ompBaseAttributes are the resource attributes a per-repo extension carries for every
// project — everything but the project id, which the extension stamps per repository.
func ompBaseAttributes(e harness.Exporter) map[string]string {
	out := map[string]string{}
	for k, v := range e.ResourceAttributes {
		if k == "" || v == "" || k == harness.AttrProjectID {
			continue
		}
		out[k] = v
	}
	return out
}

// Disconnect removes the extension and, when Terma wrote it, its helper script — or
// the repository's policy file.
func (c exporter) Disconnect() (harness.DisconnectResult, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return harness.DisconnectResult{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return harness.DisconnectResult{}, nil
	}
	if err != nil {
		return harness.DisconnectResult{}, fmt.Errorf("read %s: %w", path, err)
	}
	// A foreign hook file — no Terma config line — is not Terma's to remove. This is
	// stricter than the OpenCode plugin, whose path Terma owns outright: the hooks
	// directory is a shared namespace, so a terma.ts that did not come from a connect
	// is someone's own file.
	if c.root == "" {
		cfg, ok := readOmpExtensionConfig(data)
		if !ok {
			return harness.DisconnectResult{}, nil
		}
		if cfg.HeadersHelper != "" && harness.IsOwnHelper(cfg.HeadersHelper) {
			if err := harness.DeleteHelper(cfg.HeadersHelper); err != nil {
				return harness.DisconnectResult{}, err
			}
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return harness.DisconnectResult{}, fmt.Errorf("remove %s: %w", path, err)
	}
	return harness.DisconnectResult{Removed: 1}, nil
}

// CurrentCredential returns the key the installed extension already presents to
// endpoint for projectID, so a reconnect reuses it instead of minting an orphan.
func (c exporter) CurrentCredential(endpoint, projectID string) (string, bool) {
	if c.root != "" {
		return "", false
	}
	path, err := c.ConfigPath()
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	cfg, ok := readOmpExtensionConfig(data)
	if !ok || cfg.Endpoint != endpoint || cfg.ResourceAttributes[harness.AttrProjectID] != projectID {
		return "", false
	}
	if key := ompKey(cfg); key != "" {
		return key, true
	}
	return "", false
}

// ConnectNotes says what is particular about this harness before the user confirms.
func (c exporter) ConnectNotes(e harness.Exporter) []string {
	var notes []string
	if c.root == "" {
		notes = append(notes,
			"omp loads hooks at startup — restart it after connecting.",
			"Commit attribution runs `terma hook` from inside omp, so terma must be on the PATH omp starts with.",
			"Tokens, effort, service tier and latency ride omp's native OTLP spans; estimated cost is posted as a companion record the server joins by session.",
		)
	}
	return notes
}

// A capability asked for by type assertion switches off in silence when its method
// drifts; these make that a build error.
var (
	_ harness.Harness      = exporter{}
	_ harness.Noter        = exporter{}
	_ harness.Credentialed = exporter{}
	_ harness.Scoped       = exporter{}
)
