package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func seedConfig(t *testing.T, file *File) string {
	t.Helper()
	dir := t.TempDir()
	if file != nil {
		if err := SaveFile(dir, file); err != nil {
			t.Fatalf("SaveFile: %v", err)
		}
	}
	return dir
}

func TestLoad_PrecedenceIsFlagThenEnvThenProfile(t *testing.T) {
	dir := seedConfig(t, &File{
		ActiveProfile: DefaultProfile,
		Profiles: map[string]*Profile{
			DefaultProfile: {APIURL: "https://profile.example"},
		},
	})

	t.Run("profile supplies the value when nothing overrides it", func(t *testing.T) {
		cfg, err := Load(dir, dir, Overrides{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.APIURL != "https://profile.example" {
			t.Errorf("APIURL = %q", cfg.APIURL)
		}
		if cfg.ProjectID != "" {
			t.Errorf("ProjectID = %q", cfg.ProjectID)
		}
	})

	t.Run("environment beats the profile", func(t *testing.T) {
		t.Setenv("TERMA_API_URL", "https://env.example")
		cfg, err := Load(dir, dir, Overrides{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.APIURL != "https://env.example" {
			t.Errorf("APIURL = %q, want the environment value", cfg.APIURL)
		}
	})

	t.Run("flag beats the environment", func(t *testing.T) {
		t.Setenv("TERMA_API_URL", "https://env.example")
		cfg, err := Load(dir, dir, Overrides{APIURL: "https://flag.example"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.APIURL != "https://flag.example" {
			t.Errorf("APIURL = %q, want the flag value", cfg.APIURL)
		}
	})
}

func TestLoad_FallsBackToDefaultsWithNoConfigFile(t *testing.T) {
	// Asserts production defaults, so it must not inherit the developer's TERMA_ENV.
	t.Setenv("TERMA_ENV", "")
	dir := seedConfig(t, nil)

	cfg, err := Load(dir, dir, Overrides{})
	if err != nil {
		t.Fatalf("Load must succeed before the first login: %v", err)
	}
	if cfg.APIURL != DefaultAPIURL {
		t.Errorf("APIURL = %q, want the prod default %q", cfg.APIURL, DefaultAPIURL)
	}
	if cfg.ProfileName != DefaultProfile {
		t.Errorf("ProfileName = %q, want %q", cfg.ProfileName, DefaultProfile)
	}
}

func TestLoad_TrimsTrailingSlashFromURLs(t *testing.T) {
	t.Parallel()
	dir := seedConfig(t, nil)

	// Paths are concatenated onto these, so a trailing slash would produce //v1/...
	cfg, err := Load(dir, dir, Overrides{APIURL: "https://api.example/", AppURL: "https://app.example/"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIURL != "https://api.example" {
		t.Errorf("APIURL = %q", cfg.APIURL)
	}
	if cfg.AppURL != "https://app.example" {
		t.Errorf("AppURL = %q", cfg.AppURL)
	}
}

func TestUpdateProfile_CreatesTheProfileAndPersists(t *testing.T) {
	t.Parallel()
	dir := seedConfig(t, nil)

	if err := UpdateProfile(dir, "staging", func(p *Profile) {
		p.OrganizationID = "org-x"
		p.OrganizationName = "Organization X"
	}); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}

	file, err := LoadFile(dir)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	got := file.Profiles["staging"]
	if got == nil || got.OrganizationID != "org-x" {
		t.Fatalf("profile not persisted: %+v", got)
	}
}

// The config is private like every file terma writes: it names the developer's
// organization and team.
func TestSaveFile_WritesConfigPrivate(t *testing.T) {
	t.Parallel()
	dir := seedConfig(t, &File{ActiveProfile: DefaultProfile, Profiles: map[string]*Profile{}})

	info, err := os.Stat(filepath.Join(dir, configFileName))
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if perm := info.Mode().Perm(); runtime.GOOS != "windows" && perm != 0o600 {
		t.Errorf("config mode = %o, want 600", perm)
	}
}

// TestLoad_RefusesCleartextRemoteEndpoints: every host receives a secret, so http to a
// remote host is refused.
func TestLoad_RefusesCleartextRemoteEndpoints(t *testing.T) {
	t.Parallel()
	dir := seedConfig(t, nil)

	for _, tc := range []struct {
		name     string
		override Overrides
	}{
		{"api", Overrides{APIURL: "http://attacker.example.com"}},
		{"auth", Overrides{AuthURL: "http://attacker.example.com"}},
		{"app", Overrides{AppURL: "http://attacker.example.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(dir, dir, tc.override)
			if err == nil {
				t.Fatal("expected a cleartext remote endpoint to be refused")
			}
			if !strings.Contains(err.Error(), "cleartext") {
				t.Errorf("error should explain why, got %v", err)
			}
		})
	}
}

// TestLoad_AllowsCleartextLoopback keeps a local stack without a certificate working.
func TestLoad_AllowsCleartextLoopback(t *testing.T) {
	t.Parallel()
	dir := seedConfig(t, nil)

	for _, host := range []string{
		"http://localhost:9999",
		"http://127.0.0.1:9999",
		"http://127.0.0.2:9999", // still 127.0.0.0/8
		"http://[::1]:9999",
	} {
		t.Run(host, func(t *testing.T) {
			if _, err := Load(dir, dir, Overrides{AuthURL: host}); err != nil {
				t.Errorf("loopback %q should be allowed: %v", host, err)
			}
		})
	}
}

// TestLoad_RejectsNonHTTPSchemes: file:// would be handed to the browser opener verbatim.
func TestLoad_RejectsNonHTTPSchemes(t *testing.T) {
	t.Parallel()
	dir := seedConfig(t, nil)

	for _, raw := range []string{"file:///etc/passwd", "javascript:alert(1)", "ftp://example.com", "not-a-url"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := Load(dir, dir, Overrides{AppURL: raw}); err == nil {
				t.Errorf("expected %q to be rejected", raw)
			}
		})
	}
}

// TestLoad_DefaultsToProductionOnEveryHost: with no configuration, every endpoint is production.
func TestLoad_DefaultsToProductionOnEveryHost(t *testing.T) {
	// Asserts production defaults, so it must not inherit the developer's TERMA_ENV.
	t.Setenv("TERMA_ENV", "")
	dir := seedConfig(t, nil)

	cfg, err := Load(dir, dir, Overrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, tc := range []struct{ name, got, want string }{
		{"api", cfg.APIURL, DefaultAPIURL},
		{"auth", cfg.AuthURL, DefaultAuthURL},
		{"app", cfg.AppURL, DefaultAppURL},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestLoad_EndpointsAreOverriddenIndependently: overriding one host leaves the others alone.
func TestLoad_EndpointsAreOverriddenIndependently(t *testing.T) {
	// Asserts production defaults, so it must not inherit the developer's TERMA_ENV.
	t.Setenv("TERMA_ENV", "")
	dir := seedConfig(t, nil)

	cfg, err := Load(dir, dir, Overrides{APIURL: "https://api.self-hosted.example"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIURL != "https://api.self-hosted.example" {
		t.Errorf("APIURL = %q, want the override", cfg.APIURL)
	}
	if cfg.AuthURL != DefaultAuthURL {
		t.Errorf("AuthURL = %q, want the untouched default %q", cfg.AuthURL, DefaultAuthURL)
	}
}

// TestLoad_EndpointPrecedence pins the order: flag, environment variable, profile.
func TestLoad_EndpointPrecedence(t *testing.T) {
	dir := seedConfig(t, &File{
		ActiveProfile: DefaultProfile,
		Profiles:      map[string]*Profile{DefaultProfile: {APIURL: "https://profile.example"}},
	})

	t.Run("the profile beats the default", func(t *testing.T) {
		cfg, err := Load(dir, dir, Overrides{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.APIURL != "https://profile.example" {
			t.Errorf("APIURL = %q, want the profile value", cfg.APIURL)
		}
	})

	t.Run("TERMA_API_URL beats the profile", func(t *testing.T) {
		t.Setenv("TERMA_API_URL", "https://env.example")
		cfg, err := Load(dir, dir, Overrides{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.APIURL != "https://env.example" {
			t.Errorf("APIURL = %q, want the environment variable", cfg.APIURL)
		}
	})

	t.Run("the flag beats TERMA_API_URL", func(t *testing.T) {
		t.Setenv("TERMA_API_URL", "https://env.example")
		cfg, err := Load(dir, dir, Overrides{APIURL: "https://flag.example"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.APIURL != "https://flag.example" {
			t.Errorf("APIURL = %q, want the flag", cfg.APIURL)
		}
	})
}

func TestProfileSelectOrganization(t *testing.T) {
	t.Parallel()
	p := &Profile{OrganizationID: "org-a", OrganizationName: "Acme"}
	p.SelectOrganization("org-b", "")
	if p.OrganizationID != "org-b" || p.OrganizationName != "" {
		t.Fatalf("organization switch retained the previous name: %+v", p)
	}
	p.SelectOrganization("org-b", "Beta")
	p.SelectOrganization("org-b", "")
	if p.OrganizationName != "Beta" {
		t.Fatalf("organization name not preserved: %+v", p)
	}
}

// Concurrent UpdateFile calls keep every independent change.
func TestUpdateProfileConcurrentChoices(t *testing.T) {
	t.Parallel()
	dir := seedConfig(t, nil)
	const updates = 16
	errs := make(chan error, updates)
	start := make(chan struct{})
	for i := range updates {
		go func() {
			<-start
			errs <- UpdateProfile(dir, DefaultProfile, func(p *Profile) { p.Harnesses = append(p.Harnesses, fmt.Sprint(i)) })
		}()
	}
	close(start)
	for range updates {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	file, err := LoadFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(file.Profiles[DefaultProfile].Harnesses); got != updates {
		t.Fatalf("lost concurrent preferences: got %d, want %d", got, updates)
	}
}
