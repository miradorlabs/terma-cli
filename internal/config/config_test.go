package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedConfig(t *testing.T, file *File) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TERMA_CONFIG_DIR", dir)
	if file != nil {
		if err := SaveFile(file); err != nil {
			t.Fatalf("SaveFile: %v", err)
		}
	}
	return dir
}

func TestLoad_PrecedenceIsFlagThenEnvThenProfile(t *testing.T) {
	seedConfig(t, &File{
		ActiveProfile: DefaultProfile,
		Profiles: map[string]*Profile{
			DefaultProfile: {APIURL: "https://profile.example"},
		},
	})

	t.Run("profile supplies the value when nothing overrides it", func(t *testing.T) {
		cfg, err := Load(Overrides{})
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
		cfg, err := Load(Overrides{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.APIURL != "https://env.example" {
			t.Errorf("APIURL = %q, want the environment value", cfg.APIURL)
		}
	})

	t.Run("flag beats the environment", func(t *testing.T) {
		t.Setenv("TERMA_API_URL", "https://env.example")
		cfg, err := Load(Overrides{APIURL: "https://flag.example"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.APIURL != "https://flag.example" {
			t.Errorf("APIURL = %q, want the flag value", cfg.APIURL)
		}
	})
}

func TestLoad_FallsBackToDefaultsWithNoConfigFile(t *testing.T) {
	// Asserts production defaults, so it must not inherit the developer's TERMA_ENV:
	// the suite is run against dev, and this failed there for no reason of its own.
	t.Setenv("TERMA_ENV", "")
	seedConfig(t, nil)

	cfg, err := Load(Overrides{})
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
	seedConfig(t, nil)

	// Paths are concatenated onto these, so a trailing slash would produce //v1/...
	cfg, err := Load(Overrides{APIURL: "https://api.example/", AppURL: "https://app.example/"})
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
	seedConfig(t, nil)

	if err := UpdateProfile("staging", func(p *Profile) {
		p.OrganizationID = "org-x"
		p.OrganizationName = "Organization X"
	}); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}

	file, err := LoadFile()
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	got := file.Profiles["staging"]
	if got == nil || got.OrganizationID != "org-x" {
		t.Fatalf("profile not persisted: %+v", got)
	}
}

func TestSaveFile_WritesConfigWorldReadableButNotTheCredentialFile(t *testing.T) {
	dir := seedConfig(t, &File{ActiveProfile: DefaultProfile, Profiles: map[string]*Profile{}})

	info, err := os.Stat(filepath.Join(dir, configFileName))
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	// The config holds no secrets; the credential file (0600) is the one that does.
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("config mode = %o, want 644", perm)
	}
}

// TestLoad_RefusesCleartextRemoteEndpoints guards the CLI's most damaging misconfiguration:
// every one of these URLs carries a secret (the auth host receives the PKCE verifier and the
// refresh token; the api host receives the access token), so an http:// endpoint would put
// them on the wire in the clear with nothing else in the CLI noticing.
func TestLoad_RefusesCleartextRemoteEndpoints(t *testing.T) {
	seedConfig(t, nil)

	for _, tc := range []struct {
		name     string
		override Overrides
	}{
		{"api", Overrides{APIURL: "http://attacker.example.com"}},
		{"auth", Overrides{AuthURL: "http://attacker.example.com"}},
		{"app", Overrides{AppURL: "http://attacker.example.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(tc.override)
			if err == nil {
				t.Fatal("expected a cleartext remote endpoint to be refused")
			}
			if !strings.Contains(err.Error(), "cleartext") {
				t.Errorf("error should explain why, got %v", err)
			}
		})
	}
}

// TestLoad_AllowsCleartextLoopback keeps local development working — a local stack has no
// certificate, and its traffic never leaves the machine.
func TestLoad_AllowsCleartextLoopback(t *testing.T) {
	seedConfig(t, nil)

	for _, host := range []string{
		"http://localhost:9999",
		"http://127.0.0.1:9999",
		"http://127.0.0.2:9999", // still 127.0.0.0/8
		"http://[::1]:9999",
	} {
		t.Run(host, func(t *testing.T) {
			if _, err := Load(Overrides{AuthURL: host}); err != nil {
				t.Errorf("loopback %q should be allowed: %v", host, err)
			}
		})
	}
}

// TestLoad_RejectsNonHTTPSchemes stops a config from pointing the CLI at something that is
// not a web endpoint at all — file:// would be handed to the browser opener verbatim.
func TestLoad_RejectsNonHTTPSchemes(t *testing.T) {
	seedConfig(t, nil)

	for _, raw := range []string{"file:///etc/passwd", "javascript:alert(1)", "ftp://example.com", "not-a-url"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := Load(Overrides{AppURL: raw}); err == nil {
				t.Errorf("expected %q to be rejected", raw)
			}
		})
	}
}

// TestLoad_DefaultsToProductionOnEveryHost is the guarantee that matters now that the
// CLI ships publicly: with no configuration at all, all three endpoints are production.
// Nothing else is built in, so no user can be pointed at internal infrastructure by
// accident, and no internal hostname needs to live in this repo.
func TestLoad_DefaultsToProductionOnEveryHost(t *testing.T) {
	// Asserts production defaults, so it must not inherit the developer's TERMA_ENV:
	// the suite is run against dev, and this failed there for no reason of its own.
	t.Setenv("TERMA_ENV", "")
	seedConfig(t, nil)

	cfg, err := Load(Overrides{})
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

// TestLoad_EndpointsAreOverriddenIndependently keeps the escape hatch usable: pointing
// one host at another deployment must not silently drag the other two along, nor reset
// them.
func TestLoad_EndpointsAreOverriddenIndependently(t *testing.T) {
	// Asserts production defaults, so it must not inherit the developer's TERMA_ENV:
	// the suite is run against dev, and this failed there for no reason of its own.
	t.Setenv("TERMA_ENV", "")
	seedConfig(t, nil)

	cfg, err := Load(Overrides{APIURL: "https://api.self-hosted.example"})
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

// TestLoad_EndpointPrecedence pins the resolution order. The profile is what an internal
// developer configures once; the environment variable is what CI sets per-job; the flag
// is the one-off.
func TestLoad_EndpointPrecedence(t *testing.T) {
	seedConfig(t, &File{
		ActiveProfile: DefaultProfile,
		Profiles:      map[string]*Profile{DefaultProfile: {APIURL: "https://profile.example"}},
	})

	t.Run("the profile beats the default", func(t *testing.T) {
		cfg, err := Load(Overrides{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.APIURL != "https://profile.example" {
			t.Errorf("APIURL = %q, want the profile value", cfg.APIURL)
		}
	})

	t.Run("TERMA_API_URL beats the profile", func(t *testing.T) {
		t.Setenv("TERMA_API_URL", "https://env.example")
		cfg, err := Load(Overrides{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.APIURL != "https://env.example" {
			t.Errorf("APIURL = %q, want the environment variable", cfg.APIURL)
		}
	})

	t.Run("the flag beats TERMA_API_URL", func(t *testing.T) {
		t.Setenv("TERMA_API_URL", "https://env.example")
		cfg, err := Load(Overrides{APIURL: "https://flag.example"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.APIURL != "https://flag.example" {
			t.Errorf("APIURL = %q, want the flag", cfg.APIURL)
		}
	})
}

func TestProfileSelectOrganization(t *testing.T) {
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
