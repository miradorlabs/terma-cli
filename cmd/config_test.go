package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// TestConfigView_NeverCarriesTheAPIKey guards a leak that shipped once: `config show
// -o json` serialized the internal Config, which holds TERMA_API_KEY, printing a
// live server key into whatever consumed the output.
func TestConfigView_NeverCarriesTheAPIKey(t *testing.T) {
	view := configView{
		Profile: "default",
		APIURL:  "https://api.example",
		Auth:    "server key (TERMA_API_KEY)",
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(strings.ToLower(string(encoded)), "ter_srv_") {
		t.Fatalf("configView serialized a credential: %s", encoded)
	}
	if !strings.Contains(string(encoded), "server key") {
		t.Errorf("the view should still report which credential type is in use: %s", encoded)
	}
}

// Profiles live in a map, and a map's order is different on every run: the list
// reshuffled itself between two invocations with nothing changed. Enough profiles
// that an unsorted range would have to be lucky to come out in order.
func TestConfigProfilesAreListedByName(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	names := []string{"staging", "alpha", "work", "default", "beta", "personal", "zeta", "client"}
	for _, name := range names {
		if err := config.UpdateProfile(name, func(p *config.Profile) { p.OrganizationName = "org-" + name }); err != nil {
			t.Fatal(err)
		}
	}

	out, err := runTerma(t, "config", "profiles")
	if err != nil {
		t.Fatalf("config profiles: %v\n%s", err, out)
	}
	last := -1
	for _, name := range []string{"alpha", "beta", "client", "default", "personal", "staging", "work", "zeta"} {
		at := strings.Index(out, "org-"+name)
		if at < 0 {
			t.Fatalf("profile %s is missing:\n%s", name, out)
		}
		if at < last {
			t.Fatalf("profile %s is listed out of order:\n%s", name, out)
		}
		last = at
	}
}

// TestConfig_APIKeyIsNotSerializable is the second layer: even if a future command
// renders config.Config directly, the key must not travel with it.
func TestConfig_APIKeyIsNotSerializable(t *testing.T) {
	encoded, err := json.Marshal(&config.Config{
		ProfileName: "default",
		APIKey:      "ter_srv_secret",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "ter_srv_secret") {
		t.Fatalf("config.Config serialized the API key: %s", encoded)
	}
}
