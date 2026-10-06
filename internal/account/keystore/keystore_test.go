package keystore

import (
	"path/filepath"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
)

func TestSetForRemembersPerHarnessAndForTheSpool(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const key = "ter_srv_0123456789abcdef"
	if err := SetFor(dir, "claude", "proj-1", key, Hosts{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := GetFor(dir, "claude", "proj-1"); got != key {
		t.Fatalf("GetFor = %q", got)
	}
	if got, _ := GetFor(dir, "codex", "proj-1"); got != "" {
		t.Fatalf("another harness must not inherit the key: %q", got)
	}
	if got, _ := Get(dir, "proj-1"); got != key {
		t.Fatalf("Get = %q", got)
	}
	if err := SetFor(dir, "", "proj-1", key, Hosts{}); err == nil {
		t.Fatal("a harness name is required")
	}
	if err := SetFor(dir, "claude", "proj-1", "not-a-key", Hosts{}); err == nil {
		t.Fatal("only server keys are stored")
	}
}

// The first key creates the config directory.
func TestSetCreatesTheConfigDirectory(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "not", "there", "yet")
	const key = "ter_srv_0123456789abcdef"
	if err := Set(dir, "proj-1", key, Hosts{}); err != nil {
		t.Fatalf("the first key on a new machine: %v", err)
	}
	if got, _ := Get(dir, "proj-1"); got != key {
		t.Fatalf("Get = %q", got)
	}
}

func TestMiradorKeysAreNotReused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := save(dir, &file{Keys: map[string]string{"project": "mir_srv_1234"}, HarnessKeys: map[string]map[string]string{"codex": {"project": "mir_srv_1234"}}}); err != nil {
		t.Fatal(err)
	}
	if k, _ := Get(dir, "project"); k != "" {
		t.Fatal("Get reused a mirador key")
	}
	if k, _ := GetFor(dir, "codex", "project"); k != "" {
		t.Fatal("unsupported key was reused")
	}
	if err := Set(dir, "project", "mir_srv_1234", Hosts{}); err == nil {
		t.Fatal("unsupported key was accepted")
	}
}

// Hosts are filed with the key: re-storing it from a profile pointed elsewhere keeps them,
// and only a new key takes the caller's.
func TestHostsTravelWithTheKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const devKey, newKey = "ter_srv_0123456789abcdef", "ter_srv_fedcba9876543210"
	dev := Hosts{OTLP: "https://otel.dev.example/", API: "https://api.dev.example"}
	prod := Hosts{OTLP: "https://otel.prod.example", API: "https://api.prod.example"}

	if _, ok := HostsFor(dir, "proj-1"); ok {
		t.Fatal("nothing stored yet")
	}
	if err := SetFor(dir, "claude", "proj-1", devKey, dev); err != nil {
		t.Fatal(err)
	}
	if got, ok := HostsFor(dir, "proj-1"); !ok || got != (Hosts{OTLP: "https://otel.dev.example", API: "https://api.dev.example"}) {
		t.Fatalf("HostsFor = %+v, %v", got, ok)
	}
	if err := Set(dir, "proj-1", devKey, prod); err != nil {
		t.Fatal(err)
	}
	if got, _ := HostsFor(dir, "proj-1"); got.OTLP != "https://otel.dev.example" {
		t.Fatalf("re-storing the same key re-labelled it: %+v", got)
	}
	if err := Set(dir, "proj-1", newKey, prod); err != nil {
		t.Fatal(err)
	}
	if got, _ := HostsFor(dir, "proj-1"); got != prod {
		t.Fatalf("a new key keeps its own hosts, got %+v", got)
	}
	if err := Set(dir, "proj-1", newKey, Hosts{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := HostsFor(dir, "proj-1"); got != prod {
		t.Fatalf("storing without hosts dropped them: %+v", got)
	}
}

// A key stored without hosts gets them the first time it is stored with some.
func TestHostsFillInForAKeyStoredWithout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const key = "ter_srv_0123456789abcdef"
	if err := save(dir, &file{Keys: map[string]string{"proj-1": key}}); err != nil {
		t.Fatal(err)
	}
	want := Hosts{OTLP: "https://otel.dev.example", API: "https://api.dev.example"}
	if err := Set(dir, "proj-1", key, want); err != nil {
		t.Fatal(err)
	}
	if got, ok := HostsFor(dir, "proj-1"); !ok || got != want {
		t.Fatalf("HostsFor = %+v, %v", got, ok)
	}
}

// A built-in environment is recorded by name and read back through the current table; a
// customised profile is recorded as it is.
func TestHostsOfNamesABuiltInEnvironment(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dev, err := config.EndpointsFor(config.EnvDev)
	if err != nil {
		t.Fatal(err)
	}
	stock := HostsOf(&config.Config{Environment: config.EnvDev, OTLPURL: dev.OTLPURL, APIURL: dev.APIURL})
	if stock.Env != config.EnvDev {
		t.Fatalf("stock dev hosts = %+v, want the environment named", stock)
	}
	custom := HostsOf(&config.Config{Environment: config.EnvDev, OTLPURL: "http://127.0.0.1:4318/", APIURL: dev.APIURL})
	if custom.Env != "" || custom.OTLP != "http://127.0.0.1:4318" {
		t.Fatalf("custom hosts = %+v, want them as configured", custom)
	}

	if err := save(dir, &file{
		Keys:  map[string]string{"proj-1": "ter_srv_0123456789abcdef"},
		Hosts: map[string]Hosts{"proj-1": {Env: config.EnvDev, OTLP: "https://otel-old.example", API: "https://api-old.example"}},
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := HostsFor(dir, "proj-1"); got.OTLP != dev.OTLPURL || got.API != dev.APIURL {
		t.Fatalf("HostsFor = %+v, want the current dev hosts", got)
	}
	if err := save(dir, &file{
		Keys:  map[string]string{"proj-1": "ter_srv_0123456789abcdef"},
		Hosts: map[string]Hosts{"proj-1": {Env: "staging", OTLP: "https://otel.staging.example", API: "https://api.staging.example"}},
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := HostsFor(dir, "proj-1"); got.OTLP != "https://otel.staging.example" {
		t.Fatalf("HostsFor = %+v, want the recorded hosts", got)
	}
}
