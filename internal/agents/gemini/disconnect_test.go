package gemini

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Undoing the relay takes out only terma's telemetry settings, and none the developer pointed elsewhere since.
func TestDisconnectRelayKeepsTheDevelopersSettings(t *testing.T) {
	t.Setenv("GEMINI_CLI_HOME", t.TempDir())
	settings, err := settingsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"theme":"dark","telemetry":{"outfile":"/tmp/gemini.log"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	read := func() map[string]any {
		t.Helper()
		data, err := os.ReadFile(settings)
		if err != nil {
			t.Fatal(err)
		}
		doc := map[string]any{}
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	if _, _, err := connectRelay("http://127.0.0.1:43180/tok", []string{"terma", "hook"}); err != nil {
		t.Fatal(err)
	}
	if _, err := disconnectRelay("tok"); err != nil {
		t.Fatal(err)
	}
	doc := read()
	tel, _ := doc["telemetry"].(map[string]any)
	if doc["theme"] != "dark" || tel["outfile"] != "/tmp/gemini.log" || len(tel) != 1 {
		t.Fatalf("settings after undo: %v", doc)
	}
	if ext, _ := extensionDir(); func() bool { _, err := os.Stat(ext); return err == nil }() {
		t.Fatal("terma's extension is still there")
	}

	// Pointed elsewhere by the developer since: not terma's to remove.
	if _, _, err := connectRelay("http://127.0.0.1:43180/tok", []string{"terma", "hook"}); err != nil {
		t.Fatal(err)
	}
	doc = read()
	doc["telemetry"].(map[string]any)["otlpEndpoint"] = "https://otel.example.com"
	data, _ := json.Marshal(doc)
	if err := os.WriteFile(settings, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := disconnectRelay("tok"); err != nil {
		t.Fatal(err)
	}
	if got := read()["telemetry"].(map[string]any)["otlpEndpoint"]; got != "https://otel.example.com" {
		t.Fatalf("the developer's endpoint became %v", got)
	}
}

// Settings terma never pointed at the relay are not terma's to parse, so ones that are not JSON stay as they are.
func TestDisconnectRelayLeavesSettingsWithoutTheToken(t *testing.T) {
	t.Setenv("GEMINI_CLI_HOME", t.TempDir())
	settings, err := settingsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	mine := "// the developer's own\n{\"theme\":\"dark\"}\n"
	if err := os.WriteFile(settings, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"tok", ""} {
		if changed, err := disconnectRelay(token); err != nil || len(changed) > 0 {
			t.Fatalf("disconnectRelay(%q) = %v, %v", token, changed, err)
		}
	}
	if data, _ := os.ReadFile(settings); string(data) != mine {
		t.Fatalf("settings changed:\n%s", data)
	}
}
