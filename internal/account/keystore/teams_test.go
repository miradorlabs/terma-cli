package keystore

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
)

const otherKey = "ter_srv_fedcba9876543210"

func teamsFileText(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(config.KeysV2Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func mustNot(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// keys.v2.json indexes each enrollment's key, its team and its hosts; the key itself is
// the keychain's team-key/<enrollment>, and keys.json is never written.
func TestTeamKeysByEnrollment(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	hosts := Hosts{OTLP: "https://otel.example/", API: "https://api.example"}
	mustNot(t, SetTeam(dir, "enr-1", "acme-platform", testKey, hosts))
	f := teamsFileText(t, dir)
	if strings.Contains(f, "ter_srv_") || !strings.Contains(f, `"enr-1"`) || !strings.Contains(f, `"project": "acme-platform"`) {
		t.Fatalf("keys.v2.json must index the key without holding it:\n%s", f)
	}
	if v, err := secret.Get(dir, "team-key/enr-1"); err != nil || v != testKey {
		t.Fatalf("keychain item = %q, %v", v, err)
	}
	if _, err := os.Stat(path(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a team key wrote keys.json: %v", err)
	}
	key, got, err := GetTeam(dir, "enr-1")
	if err != nil || key != testKey || got != (Hosts{OTLP: "https://otel.example", API: "https://api.example"}) {
		t.Fatalf("GetTeam = %q, %+v, %v", key, got, err)
	}
	if key, _, err := GetTeam(dir, "enr-2"); err != nil || key != "" {
		t.Fatalf("an enrollment with no key = %q, %v", key, err)
	}
	if k, _ := Get(dir, "acme-platform"); k != "" {
		t.Fatal("a team key is visible to v1's Get")
	}

	for _, bad := range [][3]string{
		{"", "acme-platform", testKey},
		{"enr-1", "", testKey},
		{"enr-1", "acme-platform", "mir_srv_1234"},
	} {
		if err := SetTeam(dir, bad[0], bad[1], bad[2], Hosts{}); err == nil {
			t.Errorf("SetTeam accepted %q", bad)
		}
	}
	if err := SetTeam(dir, "enr-1", "clientco", otherKey, Hosts{}); err == nil {
		t.Fatal("an enrollment took another team's key")
	}
}

// Hosts travel with the key, as in keys.json: the same key keeps its hosts, a new key
// takes the caller's, and none given keeps them.
func TestTeamHostsTravelWithTheKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dev := Hosts{OTLP: "https://otel.dev.example", API: "https://api.dev.example"}
	prod := Hosts{OTLP: "https://otel.prod.example", API: "https://api.prod.example"}
	mustNot(t, SetTeam(dir, "enr-1", "p", testKey, dev))
	mustNot(t, SetTeam(dir, "enr-1", "p", testKey, prod))
	if _, h, _ := GetTeam(dir, "enr-1"); h != dev {
		t.Fatalf("re-storing the same key re-labelled it: %+v", h)
	}
	mustNot(t, SetTeam(dir, "enr-1", "p", otherKey, prod))
	if _, h, _ := GetTeam(dir, "enr-1"); h != prod {
		t.Fatalf("a new key keeps its own hosts, got %+v", h)
	}
	mustNot(t, SetTeam(dir, "enr-1", "p", otherKey, Hosts{}))
	if _, h, _ := GetTeam(dir, "enr-1"); h != prod {
		t.Fatalf("storing without hosts dropped them: %+v", h)
	}
}

// The value rules match keys.json: insecure storage, or a keychain that will not take the
// key, keeps it in the file; a locked keychain is an error, never "no key".
func TestTeamKeysWithoutAKeychain(t *testing.T) {
	t.Parallel()
	for name, prepare := range map[string]func(t *testing.T, dir string){
		"no keychain": func(t *testing.T, dir string) { secret.FailForTest(t, dir) },
		"refused":     func(t *testing.T, dir string) { secret.RefuseForTest(t, dir, "team-key/") },
		"insecure storage": func(t *testing.T, dir string) {
			mustNot(t, config.UpdateFile(dir, func(f *config.File) { f.InsecureStorage = true }))
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			prepare(t, dir)
			mustNot(t, SetTeam(dir, "enr-1", "p", testKey, Hosts{}))
			if !strings.Contains(teamsFileText(t, dir), testKey) {
				t.Fatal("the key must fall back to keys.v2.json")
			}
			if got, _, err := GetTeam(dir, "enr-1"); err != nil || got != testKey {
				t.Fatalf("GetTeam = %q, %v", got, err)
			}
		})
	}

	dir := t.TempDir()
	mustNot(t, SetTeam(dir, "enr-1", "p", testKey, Hosts{}))
	unlock := secret.FailForTest(t, dir)
	if _, _, err := GetTeam(dir, "enr-1"); !secret.IsUnavailable(err) {
		t.Fatalf("a locked keychain read as %v", err)
	}
	unlock()
	mustNot(t, os.WriteFile(config.KeysV2Path(dir), []byte("{"), 0o600))
	if _, _, err := GetTeam(dir, "enr-1"); err == nil {
		t.Fatal("an unreadable keys.v2.json read as no key")
	}
}

// Deleting an ended enrollment's key removes its item and no other: the same team ticked
// again keeps its new key.
func TestDeleteTeamTouchesOnlyItsEnrollment(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mustNot(t, SetTeam(dir, "enr-old", "acme-platform", testKey, Hosts{}))
	mustNot(t, SetTeam(dir, "enr-new", "acme-platform", otherKey, Hosts{}))
	mustNot(t, DeleteTeam(dir, "enr-old"))
	if _, err := secret.Get(dir, "team-key/enr-old"); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("the ended enrollment's item is still there: %v", err)
	}
	if got, _, err := GetTeam(dir, "enr-new"); err != nil || got != otherKey {
		t.Fatalf("the newer enrollment's key = %q, %v", got, err)
	}
	if f := teamsFileText(t, dir); strings.Contains(f, "enr-old") {
		t.Fatalf("keys.v2.json still names the ended enrollment:\n%s", f)
	}
	mustNot(t, DeleteTeam(dir, "never-there"))
}

// v1's functions and its sweep never touch a team key, and v2's never touch keys.json's.
func TestV1AndV2KeysStayApart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mustNot(t, SetFor(dir, "claude", "acme-platform", testKey, Hosts{}))
	mustNot(t, SetTeam(dir, "enr-1", "acme-platform", otherKey, Hosts{}))
	// DeleteHarnessKeys marks and sweeps v1 items.
	mustNot(t, DeleteHarnessKeys(dir, "acme-platform"))
	if got, _, _ := GetTeam(dir, "enr-1"); got != otherKey {
		t.Fatal("v1's sweep reached a team key")
	}
	mustNot(t, DeleteTeam(dir, "enr-1"))
	if got, _ := Get(dir, "acme-platform"); got != testKey {
		t.Fatal("deleting a team key reached keys.json's")
	}
}
