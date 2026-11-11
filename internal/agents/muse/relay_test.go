package muse

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

const relayToken = "0123456789abcdef0123456789abcdef0123456789abcdef"

func museSettingsAt(t *testing.T, doc string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path := filepath.Join(dir, "muse", "settings.json")
	if doc != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func readTelemetry(t *testing.T, path string) (map[string]any, map[string]any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	tel, _ := doc["telemetry"].(map[string]any)
	return doc, tel
}

var relayCfg = agents.RelayConfig{Endpoint: "http://127.0.0.1:43180", Token: relayToken}

// Setup points Muse's telemetry at the relay beside its hooks, private to the user, and teardown
// takes it out again: Muse then sends to Meta itself, as before.
func TestMuseTelemetryGoesThroughTheRelay(t *testing.T) {
	path := museSettingsAt(t, `{"schema_version":1,"hooks":{"Stop":[]},"telemetry":{"proxy":"http://proxy:3128"}}`)
	a := Agent{}
	res, err := a.ConfigureRelay(t.Context(), relayCfg)
	if err != nil {
		t.Fatal(err)
	}
	doc, tel := readTelemetry(t, path)
	if tel["endpoint"] != "http://127.0.0.1:43180/"+relayToken || tel["proxy"] != "http://proxy:3128" || doc["hooks"] == nil || len(res.Paths) != 1 {
		t.Fatalf("settings = %v, result %+v", doc, res)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("settings holding the relay's token are %v", fi.Mode().Perm())
	}
	if pointed, known := a.RelayPointed("127.0.0.1:43180"); !pointed || !known {
		t.Errorf("RelayPointed = %v, %v", pointed, known)
	}

	// A relay set up again with a new token replaces its own endpoint.
	again := relayCfg
	again.Token = strings.Repeat("f", 48)
	if _, err := a.ConfigureRelay(t.Context(), again); err != nil {
		t.Fatal(err)
	}
	if _, tel := readTelemetry(t, path); tel["endpoint"] != "http://127.0.0.1:43180/"+again.Token {
		t.Errorf("a new token left %v", tel)
	}

	if _, err := a.UnconfigureRelay(t.Context(), relayToken); err != nil {
		t.Fatal(err)
	}
	if _, tel := readTelemetry(t, path); tel["endpoint"] == nil {
		t.Error("teardown with an old token removed the current relay's endpoint")
	}
	if _, err := a.UnconfigureRelay(t.Context(), again.Token); err != nil {
		t.Fatal(err)
	}
	doc, tel = readTelemetry(t, path)
	if tel["endpoint"] != nil || tel["proxy"] != "http://proxy:3128" || doc["hooks"] == nil {
		t.Errorf("after teardown: %v", doc)
	}
}

// A developer who routes Muse's telemetry themselves, or turned it off, keeps that.
func TestMuseTelemetryOfTheDevelopersOwnIsLeftAlone(t *testing.T) {
	for _, own := range []string{
		`{"telemetry":{"endpoint":"https://otel.example.com"}}`,
		`{"telemetry":{"enabled":false}}`,
		`{"telemetry":{"destination":"edge"}}`,
	} {
		path := museSettingsAt(t, own)
		a := Agent{}
		res, err := a.ConfigureRelay(t.Context(), relayCfg)
		if err != nil {
			t.Fatal(err)
		}
		if data, _ := os.ReadFile(path); string(data) != own || len(res.Paths) != 0 || len(res.Notes) != 1 {
			t.Errorf("%s became %s (%+v)", own, data, res)
		}
		if _, known := a.RelayPointed("127.0.0.1:43180"); known {
			t.Errorf("%s: the developer's own routing was reported as not pointed at the relay", own)
		}
	}
}

// Settings that do not parse are the developer's to fix: terma writes nothing over them.
func TestMuseUnparsableSettingsAreLeftAlone(t *testing.T) {
	path := museSettingsAt(t, "{ not json")
	if _, err := (Agent{}).ConfigureRelay(t.Context(), relayCfg); err == nil {
		t.Fatal("unparsable settings were configured")
	}
	if data, _ := os.ReadFile(path); string(data) != "{ not json" {
		t.Errorf("settings became %q", data)
	}
}
