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

// exporter configures omp's export by installing a hook extension: omp's native exporter
// reads only OTEL_* process env, and files in ~/.omp/agent/hooks/pre load before it does,
// so the extension sets them. It runs terma's helper script for its key. With root set it
// is the repository's committed policy file instead, which names no destination or key.
type exporter struct {
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

	// ompConfigPlaceholder is the line a connect replaces; unspliced, the extension is inert.
	ompConfigPlaceholder = "const CONFIG = null /* terma:config */"
	ompConfigPrefix      = "const CONFIG = "

	ompCaptureContentEnv = "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT"
)

// Name is the token `--harness` accepts.
func (exporter) Name() string { return name }

// DisplayName is how the agent is written in prose.
func (exporter) DisplayName() string { return displayName }

// Local is the exporter bound to the repository at root.
func (exporter) Local(root string) (harness.Harness, bool) { return exporter{root: root}, true }

// Detect runs `omp --version`; a missing binary is not-found, not an error.
func (exporter) Detect(ctx context.Context) harness.Detection {
	return harness.DetectBinary(ctx, "omp", harness.SemverRE)
}

// ompAgentDir is $OMP_DIR/agent, else ~/.omp/agent, on every platform.
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

// ompConfig is what the user-scope extension is spliced with.
type ompConfig struct {
	Version            int               `json:"version"`
	Endpoint           string            `json:"endpoint,omitempty"`
	Headers            map[string]string `json:"headers,omitempty"`
	HeadersHelper      string            `json:"headersHelper,omitempty"`
	Signals            []string          `json:"signals"`
	IncludePrompts     bool              `json:"includePrompts"`
	IncludeToolContent bool              `json:"includeToolContent"`
	ResourceAttributes map[string]string `json:"resourceAttributes,omitempty"`

	HookCommand []string `json:"hookCommand,omitempty"`

	// PerRepo resolves the repository's binding and helper per session, with no fixed
	// destination or key.
	PerRepo          bool   `json:"perRepo,omitempty"`
	HelpersDir       string `json:"helpersDir,omitempty"`
	HelperPrefix     string `json:"helperPrefix,omitempty"`
	ProjectAttribute string `json:"projectAttribute,omitempty"`
}

// ompPolicy is the repository-scope file: what a repository may decide. Content is not
// among it; an earlier terma's includePrompts and includeToolContent are read only to be
// found stale, and a rewrite drops them.
type ompPolicy struct {
	Signals            []string `json:"signals"`
	IncludePrompts     *bool    `json:"includePrompts,omitempty"`
	IncludeToolContent *bool    `json:"includeToolContent,omitempty"`
}

// config builds what the extension reads; at repository scope only the policy fields.
func (c exporter) config(e harness.Exporter) ompConfig {
	cfg := ompConfig{
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

// readOmpExtensionConfig reads the spliced config back; a file without it is not terma's.
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

// Status reads the installed extension or policy back.
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
		status.StaleContent = policy.IncludePrompts != nil && !*policy.IncludePrompts ||
			policy.IncludeToolContent != nil && !*policy.IncludeToolContent
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
	status.ProjectID = cfg.ResourceAttributes[harness.AttrProjectID]
	status.Connected = cfg.Endpoint != "" && (cfg.HeadersHelper != "" || cfg.Headers["Authorization"] != "")
	status.KeyPrefix = harness.MaskKey(ompKey(cfg))
	status.Conflicts = ompConflicts(harness.Exporter{Endpoint: cfg.Endpoint})
	return status, nil
}

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

// Connect writes the extension and its helper, or the repository's policy file, whole:
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
		data, err := json.MarshalIndent(ompPolicy{Signals: cfg.Signals}, "", "  ")
		if err != nil {
			return err
		}
		return config.WriteFileAtomic(path, append(data, '\n'), 0o644)
	}

	// The helper first: an omp starting between the two writes must find the credential.
	if e.HelperPath != "" {
		if err := harness.WriteHelper(e.HelperPath, e.APIKey); err != nil {
			return err
		}
	}
	src, err := renderOmpExtension(cfg)
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

// ConnectPerRepo writes this project's headers helper and the shared extension in
// per-repo mode, which resolves each session's project and helper at runtime.
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
		// A direct export, past the relay that applies the team's policy, so content
		// capture stays off.
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
	return config.WriteFileAtomic(path, src, 0o644)
}

// ompBaseAttributes are e's resource attributes but the project id, stamped per repository.
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

// Disconnect removes the extension and terma's helper, or the repository's policy file.
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
	// The hooks directory is shared: a terma.ts without terma's config line is someone's own.
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

// CurrentCredential returns the key the extension presents to endpoint for projectID,
// so a reconnect reuses it instead of minting an orphan.
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

// Backup takes none.
func (exporter) Backup(string) (string, error) { return "", nil }

var (
	_ harness.Harness = exporter{}
)
