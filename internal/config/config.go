// Package config resolves CLI settings from flags, environment and the on-disk
// profile, and persists the non-secret half; tokens live apart in credentials.json (0600).
package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
)

const (
	// DefaultProfile is the profile a command uses when none is named or active.
	DefaultProfile = "default"

	configFileName      = "config.json"
	credentialsFileName = "credentials.json"
	dirName             = "terma"
)

// Profile is the non-secret half of a profile: where to talk to and what is selected.
type Profile struct {
	// Environment pins a hidden built-in environment; empty means production.
	Environment string `json:"environment,omitempty"`
	// Endpoint overrides; empty means the environment's defaults.
	APIURL           string `json:"api_url,omitempty"`
	AuthURL          string `json:"auth_url,omitempty"`
	AppURL           string `json:"app_url,omitempty"`
	OTLPURL          string `json:"otlp_url,omitempty"`
	OrganizationID   string `json:"organization_id,omitempty"`
	OrganizationName string `json:"organization_name,omitempty"`
	// Harnesses lists the agents and launch surfaces `terma setup` recorded; a preference, not a connection.
	Harnesses []string `json:"harnesses,omitempty"`
	// Policy is the collection policy `terma setup` last fetched; nil means DefaultPolicy.
	Policy *Policy `json:"policy,omitempty"`
}

// SelectOrganization records the account scope, never a repository's project.
func (p *Profile) SelectOrganization(id, name string) {
	if p.OrganizationID != id {
		p.OrganizationName = ""
		p.Policy = nil
	}
	p.OrganizationID = id
	if name != "" {
		p.OrganizationName = name
	}
}

// PinEnvironment records env as the profile's, production as none.
func (p *Profile) PinEnvironment(env string) {
	if env == EnvProd {
		env = ""
	}
	p.Environment = env
}

// File is config.json as it is on disk: every profile, and which one is active.
type File struct {
	ActiveProfile string              `json:"active_profile"`
	Profiles      map[string]*Profile `json:"profiles"`
}

// Config is the fully resolved view a command works against.
type Config struct {
	ProfileName string
	// Environment is the built-in environment the defaults came from.
	Environment string
	// ProfileEnvironment is the one the profile records, which a process without TERMA_ENV,
	// such as a hook, resolves to.
	ProfileEnvironment string
	// APIURL is the data plane; AuthURL is the credential surface, a separate host.
	APIURL  string
	AuthURL string
	AppURL  string
	// OTLPURL is the ingest host written into agents' exporters; the CLI never calls it.
	OTLPURL string

	OrganizationID   string
	OrganizationName string
	ProjectID        string
	ProjectName      string
	// ProjectOrganizationID comes from the repository binding, not the signed-in organization.
	ProjectOrganizationID string

	Harnesses []string

	// Policy is the profile's collection policy, else DefaultPolicy.
	Policy Policy

	// APIKey is a server key from TERMA_API_KEY that replaces the login credential;
	// json:"-" keeps it out of `-o json`.
	APIKey string `json:"-"`
}

// Overrides are the flag values that win over everything else.
type Overrides struct {
	Profile string
	// Env selects a built-in environment; hidden from users.
	Env       string
	APIURL    string
	AuthURL   string
	AppURL    string
	OTLPURL   string
	ProjectID string
}

// Load resolves settings in precedence order: flag, environment variable, active
// profile, built-in default (production).
func Load(o Overrides) (*Config, error) {
	file, err := LoadFile()
	if err != nil {
		return nil, err
	}

	name := firstNonEmpty(o.Profile, os.Getenv("TERMA_PROFILE"), file.ActiveProfile, DefaultProfile)
	profile := file.Profiles[name]
	if profile == nil {
		profile = &Profile{}
	}

	// The environment picks the built-in defaults; explicit URLs still win over it.
	envName := firstNonEmpty(o.Env, os.Getenv("TERMA_ENV"), profile.Environment, EnvProd)
	defaults, err := EndpointsFor(envName)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		ProfileName:        name,
		Environment:        envName,
		ProfileEnvironment: firstNonEmpty(profile.Environment, EnvProd),
		APIURL:             strings.TrimRight(firstNonEmpty(o.APIURL, os.Getenv("TERMA_API_URL"), profile.APIURL, defaults.APIURL), "/"),
		AuthURL:            strings.TrimRight(firstNonEmpty(o.AuthURL, os.Getenv("TERMA_AUTH_URL"), profile.AuthURL, defaults.AuthURL), "/"),
		AppURL:             strings.TrimRight(firstNonEmpty(o.AppURL, os.Getenv("TERMA_APP_URL"), profile.AppURL, defaults.AppURL), "/"),
		OTLPURL:            strings.TrimRight(firstNonEmpty(o.OTLPURL, os.Getenv("TERMA_OTLP_URL"), profile.OTLPURL, defaults.OTLPURL), "/"),
		OrganizationID:     firstNonEmpty(os.Getenv("TERMA_ORGANIZATION_ID"), profile.OrganizationID),
		OrganizationName:   profile.OrganizationName,
		ProjectID:          firstNonEmpty(o.ProjectID, os.Getenv("TERMA_TEAM_ID")),
		Harnesses:          profile.Harnesses,
		Policy:             DefaultPolicy(),
		APIKey:             strings.TrimSpace(os.Getenv("TERMA_API_KEY")),
	}
	if profile.Policy != nil {
		cfg.Policy = *profile.Policy
		if !cfg.Policy.AppliesTo(cfg.OrganizationID, cfg.AuthURL) {
			cfg.Policy = NoPolicy("", "")
		}
	}

	for _, endpoint := range []struct{ name, value string }{
		{"api", cfg.APIURL},
		{"auth", cfg.AuthURL},
		{"app", cfg.AppURL},
		// Agents send a server key to the OTLP host, so it is held to the same standard.
		{"otlp", cfg.OTLPURL},
	} {
		if err := validateEndpoint(endpoint.name, endpoint.value); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// validateEndpoint refuses http except to loopback: every one of these hosts receives
// a secret at some point.
func validateEndpoint(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid %s URL %q: %w", name, raw, err)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid %s URL %q: missing host", name, raw)
	}

	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf(
			"refusing to use %s URL %q: http would send your credentials in cleartext — use https (http is allowed only for localhost)",
			name, raw)
	default:
		return fmt.Errorf("invalid %s URL %q: scheme must be https", name, raw)
	}
}

func isLoopbackHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	if host != "localhost" {
		return false
	}
	// /etc/hosts or DNS can point localhost elsewhere, so every address must be loopback;
	// an unresolvable one is allowed, or offline local development would break.
	addrs, err := net.LookupHost(host)
	if err != nil {
		return true
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil || !ip.IsLoopback() {
			return false
		}
	}
	return len(addrs) > 0
}

// LoadFile reads config.json; a missing file is an empty one.
func LoadFile() (*File, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &File{ActiveProfile: DefaultProfile, Profiles: map[string]*Profile{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var file File
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if file.Profiles == nil {
		file.Profiles = map[string]*Profile{}
	}
	if file.ActiveProfile == "" {
		file.ActiveProfile = DefaultProfile
	}
	return &file, nil
}

// SaveFile writes config.json atomically, creating the config directory if needed.
func SaveFile(file *File) error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(data, '\n'), 0o644)
}

// UpdateProfile applies mutate to the named profile and persists the result.
func UpdateProfile(name string, mutate func(*Profile)) error {
	return UpdateFile(func(file *File) {
		if name == "" {
			name = file.ActiveProfile
		}
		profile := file.Profiles[name]
		if profile == nil {
			profile = &Profile{}
		}
		mutate(profile)
		file.Profiles[name] = profile
	})
}

// UpdateFile runs mutate on config.json under a sidecar lock shared with the relay's policy refresh.
func UpdateFile(mutate func(*File)) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	unlock, err := flock.Lock(ctx, filepath.Join(dir, configFileName+".lock"))
	if err != nil {
		return err
	}
	defer unlock()
	file, err := LoadFile()
	if err != nil {
		return err
	}
	mutate(file)
	return SaveFile(file)
}

// Dir is terma's config directory: TERMA_CONFIG_DIR, else $XDG_CONFIG_HOME/terma, else ~/.config/terma.
func Dir() (string, error) {
	if custom := os.Getenv("TERMA_CONFIG_DIR"); custom != "" {
		return custom, nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, dirName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", dirName), nil
}

// Path is where config.json lives, under Dir.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, configFileName), nil
}

// CredentialsPath is where credentials.json lives, under Dir.
func CredentialsPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, credentialsFileName), nil
}

// WriteFileAtomic writes a temp file and renames it, syncing file and directory so a
// rotated refresh token survives a power loss.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	return writeFileAtomic(path, data, perm, true)
}

// WriteFileAtomicNoSync is WriteFileAtomic without the syncs, for state hooks rewrite on
// every tool call: on macOS a sync is F_FULLFSYNC, ~10 ms against 0.2 ms.
func WriteFileAtomicNoSync(path string, data []byte, perm os.FileMode) error {
	return writeFileAtomic(path, data, perm, false)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode, durable bool) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	// Chmod first, so a secret is never briefly readable at the default mode.
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if durable {
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			return fmt.Errorf("sync temp file: %w", err)
		}
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := replaceFile(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	if !durable {
		return nil
	}
	// Sync the directory so the rename survives too; best effort, as not every platform can.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// WriteJSON writes a state file under the config dir: private directory, indented
// with a trailing newline, atomic and durable.
func WriteJSON(path string, v any, perm os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return WriteFileAtomic(path, append(data, '\n'), perm)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
