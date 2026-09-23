package harness

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/serverkey"
)

// OpenCode configures the OpenCode CLI's export to Terma by installing a plugin.
//
// OpenCode has a native OpenTelemetry export, but it reads the OTEL_* variables from
// the process environment only — its config file cannot set them — so there is no file
// Terma could write an endpoint and key into, short of the user's shell profile. What
// OpenCode does offer is a plugins directory it loads at startup, and a plugin API whose
// events carry everything a trace needs: sessions, user prompts, tool calls with their
// arguments and output, and each assistant message with model, provider, tokens and
// cost. So the harness is a plugin: a single dependency-free JavaScript file, embedded
// in this binary and written to that directory with a config line spliced in. The
// plugin turns the events into OTLP/JSON and posts them to Terma; the key never sits in
// the file — like Claude Code's headers helper, the plugin runs Terma's helper script
// for its Authorization header. The user's opencode.json is never touched.
//
// Commit attribution goes the same way as every other harness: the plugin hands session
// start/end and file edits to `terma hook opencode-*`, and the logic lives there.
//
// Repository scope is a policy file, <root>/.opencode/terma.json, that the plugin lays
// over its global settings: which signals ship and whether prompt and tool content go
// with them. Nothing about where or with which key, so it is safe to commit.
type OpenCode struct {
	// root, when set, is the repository whose policy file this value acts on.
	root string
}

//go:embed opencode/terma.js
var opencodePluginSource string

const (
	opencodeServiceName = "opencode"
	opencodePluginsDir  = "plugins"
	opencodePluginFile  = "terma.js"
	opencodeLocalPolicy = ".opencode/terma.json"

	// opencodeConfigPlaceholder is the one line of the embedded plugin that a connect
	// replaces. The raw file ships with CONFIG null, which makes the plugin inert, so a
	// template that somehow reached OpenCode unspliced does nothing rather than
	// something wrong.
	opencodeConfigPlaceholder = "const CONFIG = null /* terma:config */"
	opencodeConfigPrefix      = "const CONFIG = "

	opencodeAuthorizationHeader = "Authorization"
)

// opencodeConfigLine finds the spliced config in an installed plugin file.
var opencodeConfigLine = regexp.MustCompile(`(?m)^const CONFIG = (.*)$`)

// opencodeConfig is the JSON the plugin reads. Field names are the plugin's contract.
type opencodeConfig struct {
	Version            int               `json:"version"`
	Endpoint           string            `json:"endpoint,omitempty"`
	HeadersHelper      string            `json:"headersHelper,omitempty"`
	Headers            map[string]string `json:"headers,omitempty"`
	Signals            []string          `json:"signals"`
	IncludePrompts     bool              `json:"includePrompts"`
	IncludeToolContent bool              `json:"includeToolContent"`
	ResourceAttributes map[string]string `json:"resourceAttributes,omitempty"`
	HookCommand        []string          `json:"hookCommand,omitempty"`
	// Per-repo routing: when PerRepo is set the plugin resolves the project from each
	// session's repository (.terma/settings.json) rather than from a fixed Endpoint,
	// and picks that project's key from HelpersDir/<HelperPrefix><projectID>. The key
	// never sits in the plugin file — only these paths do. ProjectAttribute is the
	// resource-attribute key the plugin stamps the resolved project id on.
	PerRepo          bool   `json:"perRepo,omitempty"`
	HelpersDir       string `json:"helpersDir,omitempty"`
	HelperPrefix     string `json:"helperPrefix,omitempty"`
	ProjectAttribute string `json:"projectAttribute,omitempty"`
}

// opencodePolicy is the repository-scope file: the subset of the config a repository
// may decide.
type opencodePolicy struct {
	Signals            []string `json:"signals"`
	IncludePrompts     bool     `json:"includePrompts"`
	IncludeToolContent bool     `json:"includeToolContent"`
}

// Name is the token `terma connect` and `--harness` accept.
func (OpenCode) Name() string { return "opencode" }

// DisplayName is how the agent is written in prose.
func (OpenCode) DisplayName() string { return "OpenCode" }

// SupportsHeadersHelper is true: the plugin runs the helper script itself.
func (OpenCode) SupportsHeadersHelper() bool { return true }

// Local returns the harness bound to the repository at root.
func (OpenCode) Local(root string) Harness { return OpenCode{root: root} }

// Scope reports which layer this value acts on.
func (c OpenCode) Scope() Scope {
	if c.root != "" {
		return ScopeLocal
	}
	return ScopeGlobal
}

// Detect runs `opencode --version`. A missing binary is not-found rather than an error.
func (OpenCode) Detect(ctx context.Context) Detection {
	return detectBinary(ctx, "opencode", semverRE)
}

// opencodeConfigDir is where OpenCode keeps its global configuration and plugins:
// $XDG_CONFIG_HOME/opencode, else ~/.config/opencode, on every platform.
func opencodeConfigDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); dir != "" {
		return filepath.Join(dir, "opencode"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "opencode"), nil
}

// ConfigPath is the plugin file OpenCode loads, or the repository's policy file.
func (c OpenCode) ConfigPath() (string, error) {
	if c.root != "" {
		return filepath.Join(c.root, filepath.FromSlash(opencodeLocalPolicy)), nil
	}
	dir, err := opencodeConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, opencodePluginsDir, opencodePluginFile), nil
}

// config builds what the plugin will read. At repository scope only the policy fields
// are kept; the destination and credential are the global connect's.
func (c OpenCode) config(e Exporter) opencodeConfig {
	cfg := opencodeConfig{
		Version:            1,
		Signals:            signalNames(e.Signals),
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
		cfg.Headers = map[string]string{opencodeAuthorizationHeader: "Bearer " + e.APIKey}
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

func signalNames(signals []Signal) []string {
	out := make([]string, 0, len(signals))
	for _, s := range signals {
		out = append(out, string(s))
	}
	return out
}

func signalsFromNames(names []string) []Signal {
	var out []Signal
	for _, s := range AllSignals {
		for _, n := range names {
			if Signal(strings.ToLower(strings.TrimSpace(n))) == s {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// renderPlugin splices the config into the embedded plugin source.
func renderPlugin(cfg opencodeConfig) ([]byte, error) {
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	if strings.Count(opencodePluginSource, opencodeConfigPlaceholder) != 1 {
		return nil, errors.New("embedded OpenCode plugin has no config line to fill in — this is a build defect")
	}
	src := strings.Replace(opencodePluginSource, opencodeConfigPlaceholder, opencodeConfigPrefix+string(encoded), 1)
	return []byte(src), nil
}

// readPluginConfig parses the config out of an installed plugin file. A file with a
// null config, or none, is reported as absent rather than as an error: it is inert.
func readPluginConfig(data []byte) (*opencodeConfig, bool) {
	m := opencodeConfigLine.FindSubmatch(data)
	if m == nil {
		return nil, false
	}
	raw := strings.TrimSpace(string(m[1]))
	raw = strings.TrimSuffix(raw, ";")
	if idx := strings.Index(raw, "/*"); idx >= 0 {
		raw = strings.TrimSpace(raw[:idx])
	}
	if raw == "" || raw == "null" {
		return nil, false
	}
	var cfg opencodeConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, false
	}
	return &cfg, true
}

// Status reads the installed plugin back.
func (c OpenCode) Status() (Status, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return Status{}, err
	}
	status := Status{ConfigPath: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return status, nil
	}
	if err != nil {
		return Status{}, fmt.Errorf("read %s: %w", path, err)
	}
	status.Exists = true

	if c.root != "" {
		var policy opencodePolicy
		if json.Unmarshal(data, &policy) != nil {
			return status, nil
		}
		status.HasPolicy = true
		status.Signals = signalsFromNames(policy.Signals)
		status.IncludePrompts = policy.IncludePrompts
		status.IncludeToolContent = policy.IncludeToolContent
		status.ManagedKeys = 1
		return status, nil
	}

	cfg, ok := readPluginConfig(data)
	if !ok {
		return status, nil
	}
	status.ManagedKeys = 1
	status.Endpoint = cfg.Endpoint
	status.Signals = signalsFromNames(cfg.Signals)
	status.IncludePrompts = cfg.IncludePrompts
	status.IncludeToolContent = cfg.IncludeToolContent
	status.ProjectID = cfg.ResourceAttributes[AttrProjectID]
	// Connected means a destination and a way to authenticate to it.
	status.Connected = cfg.Endpoint != "" && (cfg.HeadersHelper != "" || cfg.Headers[opencodeAuthorizationHeader] != "")
	status.KeyPrefix = MaskKey(opencodeKey(cfg))
	status.Conflicts = opencodeConflicts(Exporter{Endpoint: cfg.Endpoint})
	return status, nil
}

// opencodeKey is the raw key a config presents, from the helper or inline.
func opencodeKey(cfg *opencodeConfig) string {
	if cfg.HeadersHelper != "" && isOwnHelper(cfg.HeadersHelper) {
		if key := keyFromHelper(cfg.HeadersHelper); key != "" {
			return key
		}
	}
	if v := cfg.Headers[opencodeAuthorizationHeader]; v != "" {
		return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "Bearer"))
	}
	return ""
}

// ConflictsWith reports what would confuse this export. OpenCode's own OpenTelemetry
// export is driven by OTEL_EXPORTER_OTLP_ENDPOINT in the shell; it does not affect the
// plugin, and the plugin does not affect it, but a user who sees two streams should be
// told where the second one comes from. Advisory: nothing here can disclose Terma's key.
func (c OpenCode) ConflictsWith(e Exporter) ([]Conflict, error) {
	return opencodeConflicts(e), nil
}

func opencodeConflicts(e Exporter) []Conflict {
	value := os.Getenv(otelEndpoint)
	if value == "" || value == e.Endpoint {
		return nil
	}
	return []Conflict{{
		Key:       otelEndpoint,
		Value:     value,
		Reason:    "exported in your shell — OpenCode's built-in OpenTelemetry export sends its own traces there as well; Terma's plugin is unaffected",
		Scope:     ScopeEnvironment,
		Clearable: false,
		Advisory:  true,
	}}
}

// Connect writes the plugin (and its helper script), or the repository's policy file.
// The file is generated whole, so there is nothing to merge and nothing to journal:
// disconnect removes exactly this file.
func (c OpenCode) Connect(e Exporter, _ bool) error {
	path, err := c.ConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	cfg := c.config(e)

	if c.root != "" {
		data, err := json.MarshalIndent(opencodePolicy{
			Signals: cfg.Signals, IncludePrompts: cfg.IncludePrompts, IncludeToolContent: cfg.IncludeToolContent,
		}, "", "  ")
		if err != nil {
			return err
		}
		return config.WriteFileAtomic(path, append(data, '\n'), 0o644)
	}

	// The helper first: the plugin about to be written points at it, and an OpenCode
	// starting between the two writes must find the credential already there.
	if e.HelperPath != "" {
		if err := writeHelper(e.HelperPath, e.APIKey); err != nil {
			return err
		}
	}
	src, err := renderPlugin(cfg)
	if err != nil {
		return err
	}
	// 0600 only when the key is inline; with the helper the file holds a path, not a
	// secret, and stays readable like the user's other plugins.
	mode := fs.FileMode(0o644)
	if len(cfg.Headers) > 0 {
		mode = settingsMode
	}
	return config.WriteFileAtomic(path, src, mode)
}

// ConnectPerRepo sets up OpenCode to export per repository: it writes this project's
// headers helper (holding the key), and installs the global plugin in per-repo mode so
// that a session in any bound repository reports to that repository's project.
//
// The plugin file is global and shared across projects; re-running this for another
// project rewrites it (idempotently) and adds that project's helper. The plugin carries
// no key and no fixed project — only the endpoint, the helpers directory and the naming
// convention it resolves a project's helper with at runtime.
func (OpenCode) ConnectPerRepo(e Exporter) error {
	helper, err := HelperFilePath(OpenCode{}, e.ProjectID)
	if err != nil {
		return err
	}
	if err := writeHelper(helper, e.APIKey); err != nil {
		return err
	}
	helpersDir, err := HelpersDir()
	if err != nil {
		return err
	}
	cfg := opencodeConfig{
		Version:  1,
		Endpoint: e.Endpoint,
		Signals:  signalNames(e.Signals),
		// The plugin file is global and shared across every bound repository, so this
		// repo's content-capture choice must NOT ride in it — otherwise installing one
		// project would flip prompt / tool-content capture on for every other project
		// that has no policy of its own. Content capture is off in the shared floor and
		// is opt-in per repository through its committed .opencode/terma.json overlay
		// (which the plugin lays over these defaults for that repository only).
		IncludePrompts:     false,
		IncludeToolContent: false,
		ResourceAttributes: opencodeBaseAttributes(e),
		HookCommand:        []string{"terma", "hook"},
		PerRepo:            true,
		HelpersDir:         helpersDir,
		HelperPrefix:       OpenCode{}.Name() + "-otel-",
		ProjectAttribute:   AttrProjectID,
	}
	path, err := (OpenCode{}).ConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	src, err := renderPlugin(cfg)
	if err != nil {
		return err
	}
	// The key lives in the helper, not the plugin, so this file stays readable.
	return config.WriteFileAtomic(path, src, 0o644)
}

// opencodeBaseAttributes are the resource attributes a per-repo plugin carries for every
// project — everything but the project id, which the plugin stamps per repository.
func opencodeBaseAttributes(e Exporter) map[string]string {
	out := map[string]string{}
	for k, v := range e.ResourceAttributes {
		if k == "" || v == "" || k == AttrProjectID {
			continue
		}
		out[k] = v
	}
	return out
}

// Disconnect removes the plugin and, when Terma wrote it, its helper script — or the
// repository's policy file.
func (c OpenCode) Disconnect() (DisconnectResult, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return DisconnectResult{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return DisconnectResult{}, nil
	}
	if err != nil {
		return DisconnectResult{}, fmt.Errorf("read %s: %w", path, err)
	}
	if c.root == "" {
		if cfg, ok := readPluginConfig(data); ok && cfg.HeadersHelper != "" && isOwnHelper(cfg.HeadersHelper) {
			if err := deleteHelper(cfg.HeadersHelper); err != nil {
				return DisconnectResult{}, err
			}
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return DisconnectResult{}, fmt.Errorf("remove %s: %w", path, err)
	}
	return DisconnectResult{Removed: 1}, nil
}

// CurrentCredential returns the key the installed plugin already presents to endpoint
// for projectID, so a reconnect reuses it instead of minting an orphan.
func (c OpenCode) CurrentCredential(endpoint, projectID string) (string, bool) {
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
	cfg, ok := readPluginConfig(data)
	if !ok || cfg.Endpoint != endpoint || cfg.ResourceAttributes[AttrProjectID] != projectID {
		return "", false
	}
	if key := opencodeKey(cfg); serverkey.Is(key) {
		return key, true
	}
	return "", false
}

// ConnectNotes says what is particular about this harness before the user confirms.
func (c OpenCode) ConnectNotes(e Exporter) []string {
	var notes []string
	if e.HasSignal(SignalMetrics) {
		notes = append(notes, "OpenCode's plugin sends traces and events only; token counts and cost ride on each model-call span, so there is no separate metrics stream.")
	}
	if c.root == "" {
		notes = append(notes,
			"OpenCode loads plugins at startup — restart it after connecting.",
			"Commit attribution runs `terma hook` from inside OpenCode, so terma must be on the PATH OpenCode starts with.")
	}
	return notes
}
