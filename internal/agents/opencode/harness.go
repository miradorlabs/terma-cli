package opencode

import (
	"bytes"
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

	"github.com/miradorlabs/terma-cli/internal/account/serverkey"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// exporter configures OpenCode's export by installing a plugin: OpenCode's native export
// reads only OTEL_* process env, so terma's plugin turns its events into OTLP/JSON and
// runs terma's helper script for its key. With root set it is the repository's committed
// policy file instead, which names no destination or key.
type exporter struct {
	root string
}

//go:embed plugin/terma.js
var opencodePluginSource string

const (
	opencodePluginsDir  = "plugins"
	opencodePluginFile  = "terma.js"
	opencodeLocalPolicy = ".opencode/terma.json"

	// opencodeConfigPlaceholder is the line a connect replaces; unspliced, the plugin is inert.
	opencodeConfigPlaceholder = "const CONFIG = null /* terma:config */"
	opencodeConfigPrefix      = "const CONFIG = "

	opencodeAuthorizationHeader = "Authorization"
)

var opencodeConfigLine = regexp.MustCompile(`(?m)^const CONFIG = (.*)$`)

// opencodeConfig is the JSON the plugin reads; field names are the plugin's contract.
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
	// PerRepo resolves each session's project from its repository's binding and its key
	// from HelpersDir/<HelperPrefix><projectID>, stamped on ProjectAttribute.
	PerRepo          bool   `json:"perRepo,omitempty"`
	HelpersDir       string `json:"helpersDir,omitempty"`
	HelperPrefix     string `json:"helperPrefix,omitempty"`
	ProjectAttribute string `json:"projectAttribute,omitempty"`
}

// opencodePolicy is the repository-scope file: what a repository may decide. Content is not
// among it; an earlier terma's includePrompts and includeToolContent are read only to be
// found stale, and a rewrite drops them.
type opencodePolicy struct {
	Signals            []string `json:"signals"`
	IncludePrompts     *bool    `json:"includePrompts,omitempty"`
	IncludeToolContent *bool    `json:"includeToolContent,omitempty"`
}

// Name is the token `--harness` accepts.
func (exporter) Name() string { return name }

// DisplayName is how the agent is written in prose.
func (exporter) DisplayName() string { return displayName }

// Local is the exporter bound to the repository at root.
func (exporter) Local(root string) (harness.Harness, bool) { return exporter{root: root}, true }

// Detect runs `opencode --version`; a missing binary is not-found, not an error.
func (exporter) Detect(ctx context.Context) harness.Detection {
	return harness.DetectBinary(ctx, "opencode", harness.SemverRE)
}

// opencodeConfigDir is $XDG_CONFIG_HOME/opencode, else ~/.config/opencode, on every platform.
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
func (c exporter) ConfigPath() (string, error) {
	if c.root != "" {
		return filepath.Join(c.root, filepath.FromSlash(opencodeLocalPolicy)), nil
	}
	dir, err := opencodeConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, opencodePluginsDir, opencodePluginFile), nil
}

// config builds what the plugin reads; at repository scope only the policy fields.
func (c exporter) config(e harness.Exporter) opencodeConfig {
	cfg := opencodeConfig{
		Version:            1,
		Signals:            harness.SignalNames(e.Signals),
		IncludePrompts:     true,
		IncludeToolContent: true,
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

// readPluginConfig parses an installed plugin's config; a null or missing one is inert,
// not an error.
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

// Status reads the installed plugin or policy back.
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
		var policy opencodePolicy
		if json.Unmarshal(data, &policy) != nil {
			return status, nil
		}
		status.HasPolicy = true
		status.Signals = harness.SignalsFromNames(policy.Signals)
		status.StaleContent = policy.IncludePrompts != nil && !*policy.IncludePrompts ||
			policy.IncludeToolContent != nil && !*policy.IncludeToolContent
		status.ManagedKeys = 1
		return status, nil
	}

	cfg, ok := readPluginConfig(data)
	if !ok {
		return status, nil
	}
	status.ManagedKeys = 1
	status.Endpoint = cfg.Endpoint
	status.Signals = harness.SignalsFromNames(cfg.Signals)
	status.ProjectID = cfg.ResourceAttributes[harness.AttrProjectID]
	status.Connected = cfg.Endpoint != "" && (cfg.HeadersHelper != "" || cfg.Headers[opencodeAuthorizationHeader] != "")
	status.KeyPrefix = harness.MaskKey(opencodeKey(cfg))
	status.Conflicts = opencodeConflicts(harness.Exporter{Endpoint: cfg.Endpoint})
	return status, nil
}

func opencodeKey(cfg *opencodeConfig) string {
	if cfg.HeadersHelper != "" && harness.IsOwnHelper(cfg.HeadersHelper) {
		if key := harness.KeyFromHelper(cfg.HeadersHelper); key != "" {
			return key
		}
	}
	if v := cfg.Headers[opencodeAuthorizationHeader]; v != "" {
		return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "Bearer"))
	}
	return ""
}

// ConflictsWith reports, as advisory, a shell OTEL_EXPORTER_OTLP_ENDPOINT that sends
// OpenCode's own export elsewhere beside the plugin's.
func (c exporter) ConflictsWith(e harness.Exporter) ([]harness.Conflict, error) {
	return opencodeConflicts(e), nil
}

func opencodeConflicts(e harness.Exporter) []harness.Conflict {
	value := os.Getenv(harness.EnvOTLPEndpoint)
	if value == "" || value == e.Endpoint {
		return nil
	}
	return []harness.Conflict{{
		Key:       harness.EnvOTLPEndpoint,
		Value:     value,
		Reason:    "exported in your shell — OpenCode's built-in OpenTelemetry export sends its own traces there as well; Terma's plugin is unaffected",
		Scope:     harness.ScopeEnvironment,
		Clearable: false,
		Advisory:  true,
	}}
}

// Connect writes the plugin and its helper, or the repository's policy file, whole:
// nothing to merge or journal.
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
		data, err := json.MarshalIndent(opencodePolicy{Signals: cfg.Signals}, "", "  ")
		if err != nil {
			return err
		}
		return config.WriteFileAtomic(path, append(data, '\n'), 0o644)
	}

	// The helper first: an OpenCode starting between the two writes must find the credential.
	if e.HelperPath != "" {
		if err := harness.WriteHelper(e.HelperPath, e.APIKey); err != nil {
			return err
		}
	}
	src, err := renderPlugin(cfg)
	if err != nil {
		return err
	}
	// 0600 only when the key is inline; with the helper the file holds no secret.
	mode := fs.FileMode(0o644)
	if len(cfg.Headers) > 0 {
		mode = harness.SettingsMode
	}
	return config.WriteFileAtomic(path, src, mode)
}

// ConnectPerRepo writes this project's headers helper and the shared plugin in per-repo
// mode, which resolves each session's project and helper at runtime.
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
	cfg := opencodeConfig{
		Version:  1,
		Endpoint: e.Endpoint,
		Signals:  harness.SignalNames(e.Signals),
		// A direct export, past the relay that applies the team's policy, so content
		// capture stays off.
		IncludePrompts:     false,
		IncludeToolContent: false,
		ResourceAttributes: opencodeBaseAttributes(e),
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
	src, err := renderPlugin(cfg)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(path, src, 0o644)
}

// RefreshPlugin re-splices an installed plugin's configuration into this build's source,
// leaving an absent or inert plugin, and the file mode, alone.
func (exporter) RefreshPlugin() (string, bool, error) {
	path, err := (exporter{}).ConfigPath()
	if err != nil {
		return "", false, err
	}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return path, false, nil
	}
	if err != nil {
		return path, false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return path, false, err
	}
	cfg, ok := readPluginConfig(data)
	if !ok {
		return path, false, nil
	}
	src, err := renderPlugin(*cfg)
	if err != nil || bytes.Equal(src, data) {
		return path, false, err
	}
	return path, true, config.WriteFileAtomic(path, src, info.Mode().Perm())
}

// opencodeBaseAttributes are e's resource attributes but the project id, stamped per repository.
func opencodeBaseAttributes(e harness.Exporter) map[string]string {
	out := map[string]string{}
	for k, v := range e.ResourceAttributes {
		if k == "" || v == "" || k == harness.AttrProjectID {
			continue
		}
		out[k] = v
	}
	return out
}

// Disconnect removes the plugin and terma's helper, or the repository's policy file.
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
	if c.root == "" {
		if cfg, ok := readPluginConfig(data); ok && cfg.HeadersHelper != "" && harness.IsOwnHelper(cfg.HeadersHelper) {
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

// CurrentCredential returns the key the plugin presents to endpoint for projectID, so a
// reconnect reuses it instead of minting an orphan.
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
	cfg, ok := readPluginConfig(data)
	if !ok || cfg.Endpoint != endpoint || cfg.ResourceAttributes[harness.AttrProjectID] != projectID {
		return "", false
	}
	if key := opencodeKey(cfg); serverkey.Is(key) {
		return key, true
	}
	return "", false
}

// Backup takes none.
func (exporter) Backup(string) (string, error) { return "", nil }

var (
	_ harness.Harness = exporter{}
)
