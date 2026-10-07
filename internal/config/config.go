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
	"runtime"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
)

// SetupDir is the config directory's folder of what setup changed in other tools' files,
// and what it replaced there.
const SetupDir = "setup"

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
	// Team is the team `terma setup` selected, whose policy (PoliciesDir) the hooks apply.
	Team string `json:"team,omitempty"`
}

// SelectOrganization records the account scope, never a repository's project.
func (p *Profile) SelectOrganization(id, name string) {
	if p.OrganizationID != id {
		p.OrganizationName = ""
		p.Team = ""
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

// File is config.json as it is on disk: every profile, which one is active, and the
// settings that apply to the whole installation.
type File struct {
	ActiveProfile string              `json:"active_profile"`
	Profiles      map[string]*Profile `json:"profiles"`
	// AutoUpdate replaces terma with each new release, unless set false: then terma only
	// says one is out. Unset is on.
	AutoUpdate *bool `json:"auto_update,omitempty"`
	// InsecureStorage keeps new secrets in the config directory's files rather than the
	// system keychain: `terma setup --insecure-storage`, or a setup that found no keychain
	// to use. Each setup decides it afresh.
	InsecureStorage bool `json:"insecure_storage,omitempty"`
}

// Config is the fully resolved view a command works against.
type Config struct {
	// Dir is the config directory it was loaded from; StateDir is the state directory,
	// which keeps the policies (PoliciesDir).
	Dir         string
	StateDir    string
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

	Harnesses []string
	// Team is the team the profile selected, kept even while its policy is not stored.
	Team string

	// Policy is the profile's team's policy stored in the state directory, else
	// DefaultPolicy; one for another environment, or unreadable, is NoPolicy. Other
	// teams' policies, of this organization or another, are the collection's
	// (routing.Collection): hooks admit a repository by any of them.
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

// Load resolves the settings under the config directory dir in precedence order: flag,
// environment variable, active profile, built-in default (production); and the profile's
// team's policy stored under the state directory stateDir.
func Load(dir, stateDir string, o Overrides) (*Config, error) {
	file, err := LoadFile(dir)
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
		Dir:                dir,
		StateDir:           stateDir,
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
		Team:               profile.Team,
		Policy:             DefaultPolicy(),
		APIKey:             strings.TrimSpace(os.Getenv("TERMA_API_KEY")),
	}
	switch stored, ok, err := ReadPolicy(stateDir, profile.Team); {
	case err != nil || ok && !stored.SameEnvironment(cfg.AuthURL):
		cfg.Policy = NoPolicy("", "")
	case ok:
		cfg.Policy = stored
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

// InsecureStorage reports whether new secrets under dir go to files rather than the
// system keychain.
func InsecureStorage(dir string) bool {
	f, err := LoadFile(dir)
	return err == nil && f.InsecureStorage
}

// LoadFile reads config.json under dir; a missing file is an empty one.
func LoadFile(dir string) (*File, error) {
	path := Path(dir)
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

// SaveFile writes config.json under dir atomically, creating dir if needed.
func SaveFile(dir string, file *File) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(Path(dir), append(data, '\n'), 0o600)
}

// UpdateProfile applies mutate to the named profile under dir and persists the result.
func UpdateProfile(dir, name string, mutate func(*Profile)) error {
	return UpdateFile(dir, func(file *File) {
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

// UpdateFile runs mutate on dir's config.json under a sidecar lock.
func UpdateFile(dir string, mutate func(*File)) error {
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
	file, err := LoadFile(dir)
	if err != nil {
		return err
	}
	mutate(file)
	return SaveFile(dir, file)
}

// Dir resolves terma's config directory, which holds its settings and credentials:
// TERMA_CONFIG_DIR, else $XDG_CONFIG_HOME/terma, else %APPDATA%\terma on Windows, else
// ~/.config/terma. Only the command line's entry calls it; everything else is handed the result.
func Dir() (string, error) {
	if custom := os.Getenv("TERMA_CONFIG_DIR"); custom != "" {
		return custom, nil
	}
	return userDir("XDG_CONFIG_HOME", "APPDATA", ".config")
}

// StateDir resolves terma's state directory, which holds what it writes as it runs: the
// spool, the relay and the hooks' state. It is TERMA_STATE_DIR; else, when TERMA_CONFIG_DIR
// is set, that same directory, so a sandboxed terma keeps everything in one folder; else
// DefaultStateDir. Only the command line's entry calls it; everything else is handed the result.
func StateDir() (string, error) {
	for _, key := range []string{"TERMA_STATE_DIR", "TERMA_CONFIG_DIR"} {
		if custom := os.Getenv(key); custom != "" {
			return custom, nil
		}
	}
	return DefaultStateDir()
}

// DefaultStateDir is the state directory when no TERMA_ variable names one:
// $XDG_STATE_HOME/terma, else %LOCALAPPDATA%\terma on Windows, else ~/.local/state/terma.
func DefaultStateDir() (string, error) {
	return userDir("XDG_STATE_HOME", "LOCALAPPDATA", ".local", "state")
}

// userDir is terma's folder under $xdg, else on Windows under %windows%, else under the
// home directory's path rel.
func userDir(xdg, windows string, rel ...string) (string, error) {
	if dir := os.Getenv(xdg); dir != "" {
		return filepath.Join(dir, dirName), nil
	}
	if dir := os.Getenv(windows); dir != "" && runtime.GOOS == "windows" {
		return filepath.Join(dir, dirName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(append(append([]string{home}, rel...), dirName)...), nil
}

// Path is where config.json lives under the config directory dir.
func Path(dir string) string { return filepath.Join(dir, configFileName) }

// CredentialsPath is where credentials.json lives under the config directory dir.
func CredentialsPath(dir string) string { return filepath.Join(dir, credentialsFileName) }

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
