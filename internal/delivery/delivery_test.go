package delivery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

const testKey = "ter_srv_minted0123456789abcdefghijklmnopqrstuv"

// A project's events go where its key works: a pinned host, else the key's own, else the
// routing record's, else the profile's. The data API follows the record only when the
// record names another built-in environment.
func TestEachProjectGoesToItsKeysOwnEnvironment(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	prod, err := config.EndpointsFor(config.EnvProd)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := config.EndpointsFor(config.EnvDev)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{OTLPURL: prod.OTLPURL, APIURL: prod.APIURL}
	if err := keystore.Set("keyed", testKey, keystore.Hosts{OTLP: "https://otlp.keyed", API: "https://api.keyed"}); err != nil {
		t.Fatal(err)
	}
	// A record naming one host while the key names another: the key wins.
	if err := routing.SaveRecord(routing.Record{ProjectID: "keyed", Endpoint: dev.OTLPURL}); err != nil {
		t.Fatal(err)
	}
	if err := routing.SaveRecord(routing.Record{ProjectID: "routed", Endpoint: dev.OTLPURL + "/"}); err != nil {
		t.Fatal(err)
	}
	r := Router{}
	for _, tc := range []struct{ project, otlp, api string }{
		{"keyed", "https://otlp.keyed", "https://api.keyed"},
		{"routed", dev.OTLPURL, dev.APIURL},
		{"unknown", prod.OTLPURL, prod.APIURL},
	} {
		if got := r.Endpoint(cfg, tc.project); got != tc.otlp {
			t.Errorf("Endpoint(%s) = %q, want %q", tc.project, got, tc.otlp)
		}
		if got := r.API(cfg, tc.project); got != tc.api {
			t.Errorf("API(%s) = %q, want %q", tc.project, got, tc.api)
		}
	}
	pinned := Router{OTLPPinned: true, APIPinned: true}
	if pinned.Endpoint(cfg, "keyed") != cfg.OTLPURL || pinned.API(cfg, "keyed") != cfg.APIURL {
		t.Fatal("a pinned host was not used for every project")
	}
}

// A reply or a thread's name leaves only when its agent consents; with no one to ask, or
// an agent that refuses, it is withheld.
func TestConversationContentNeedsItsAgentsConsent(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	pol := config.Policy{Mode: config.ModeRepo, IncludePrompts: true, Signals: []string{"logs"}}
	reply := spool.Event{Name: hookrun.EventAssistantMessage, Attrs: map[string]any{hookrun.AttrTool: "fake"}}
	var asked []string
	for _, tc := range []struct {
		name    string
		consent func(string, hookrun.Consent) bool
		want    bool
	}{
		{"no one to ask", nil, false},
		{"refused", func(string, hookrun.Consent) bool { return false }, false},
		{"consented", func(tool string, _ hookrun.Consent) bool { asked = append(asked, tool); return true }, true},
	} {
		if got := (Router{Consent: tc.consent}).Allowed(pol, "p1", reply); got != tc.want {
			t.Errorf("%s: Allowed = %v", tc.name, got)
		}
	}
	if len(asked) != 1 || asked[0] != "fake" {
		t.Fatalf("asked %v", asked)
	}
	noPrompts := pol
	noPrompts.IncludePrompts = false
	if (Router{Consent: func(string, hookrun.Consent) bool { return true }}).Allowed(noPrompts, "p1", reply) {
		t.Fatal("a reply left under a policy that withholds prompts")
	}
}

// A queued event is held to the policy in force when it leaves: a project whose record
// cannot be read, or whose coverage moved out of global mode, sends nothing.
func TestQueuedEventsMeetTodaysPolicy(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	start := spool.Event{Name: hookrun.EventSessionStart}
	logs := config.Policy{Mode: config.ModeRepo, Signals: []string{"logs"}}
	r := Router{}
	if !r.Allowed(logs, "p1", start) {
		t.Fatal("an unrecorded project's event was withheld")
	}
	dir, err := routing.Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(dir, "p2.json"), "{"); err != nil {
		t.Fatal(err)
	}
	if r.Allowed(logs, "p2", start) {
		t.Fatal("an unreadable routing record let an event through")
	}
	global := spool.Event{Name: hookrun.EventSessionStart, Global: true}
	if r.Allowed(logs, "p1", global) {
		t.Fatal("a global-mode event left after coverage moved back to repositories")
	}
}

// A project with no policy yet, or no key, keeps its events; one with neither problem
// delivers, and every counter says why.
func TestAProjectWithoutAPolicyOrAKeyKeepsItsEvents(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	s, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"unfetched", "keyless", ""} {
		if err := s.Append(spool.Event{Name: hookrun.EventSessionStart, Attrs: map[string]any{hookrun.AttrProjectID: id}}); err != nil {
			t.Fatal(err)
		}
	}
	r := Router{Policy: func(_ context.Context, _ *config.Config, team string) (config.Policy, error) {
		if team == "unfetched" {
			return config.Policy{}, errors.New("not fetched yet")
		}
		return config.Policy{Mode: config.ModeRepo, Signals: []string{"logs"}}, nil
	}}
	res := r.Flush(t.Context(), s, &config.Config{}, true, 0)
	if res.Sent != 0 || res.Held != 2 || res.Unroutable != 1 || res.Err != nil {
		t.Fatalf("Flush = %+v", res)
	}
}

func writeFile(path, data string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(data), 0o600)
}
