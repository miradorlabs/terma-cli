package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// TestConfigView_NeverCarriesTheAPIKey proves `config show -o json` never prints TERMA_API_KEY.
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

// Profiles live in a map; enough of them that an unsorted range would have to be lucky
// to come out in order.
func TestConfigProfilesAreListedByName(t *testing.T) {
	useConfigDir(t, t.TempDir())
	names := []string{"staging", "alpha", "work", "default", "beta", "personal", "zeta", "client"}
	for _, name := range names {
		if err := config.UpdateProfile(testApp.dir, name, func(p *config.Profile) { p.OrganizationName = "org-" + name }); err != nil {
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

// TestConfig_APIKeyIsNotSerializable proves config.Config itself never serializes the key.
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
