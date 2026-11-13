package keystore

import (
	"errors"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/account/secret"
)

// legacyKeys is a machine before the migration: two teams' keys, an agent's own key for
// one, and the hosts of each.
func legacyKeys(t *testing.T, dir string) {
	t.Helper()
	mustNot(t, SetFor(dir, "claude", "acme-platform", testKey, Hosts{OTLP: "https://otel.acme.example", API: "https://api.acme.example"}))
	mustNot(t, Set(dir, "old-team", otherKey, Hosts{OTLP: "https://otel.old.example"}))
}

// The copy moves each mapped team's key and hosts under its enrollment, drops harness
// keys and unmapped teams, leaves keys.json as it was, and is safe to repeat.
func TestCopyLegacyToTeams(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	legacyKeys(t, dir)
	legacy := map[string]string{"acme-platform": "enr-acme", "no-key": "enr-none"}
	mustNot(t, CopyLegacyToTeams(dir, legacy))
	key, hosts, err := GetTeam(dir, "enr-acme")
	if err != nil || key != testKey || hosts.OTLP != "https://otel.acme.example" {
		t.Fatalf("GetTeam = %q, %+v, %v", key, hosts, err)
	}
	if v, err := secret.Get(dir, "team-key/enr-acme"); err != nil || v != testKey {
		t.Fatalf("the copy's keychain item = %q, %v", v, err)
	}
	f := teamsFileText(t, dir)
	for _, absent := range []string{"old-team", "enr-none", "claude"} {
		if strings.Contains(f, absent) {
			t.Errorf("keys.v2.json holds %s:\n%s", absent, f)
		}
	}
	if got, _ := GetFor(dir, "claude", "acme-platform"); got != testKey {
		t.Fatal("the copy changed keys.json")
	}

	// A key created for the enrollment since is kept: a repeat never overwrites it.
	mustNot(t, SetTeam(dir, "enr-acme", "acme-platform", otherKey, Hosts{}))
	mustNot(t, CopyLegacyToTeams(dir, legacy))
	if got, _, _ := GetTeam(dir, "enr-acme"); got != otherKey {
		t.Fatalf("a repeated copy overwrote the enrollment's key: %q", got)
	}
	if err := CopyLegacyToTeams(dir, map[string]string{"old-team": "enr-acme"}); err == nil {
		t.Fatal("copied another team's key into an enrollment")
	}
	if err := CopyLegacyToTeams(dir, map[string]string{"": "enr-x"}); err == nil {
		t.Fatal("copied an empty project")
	}
}

// A keychain that cannot give up a legacy key fails the copy and leaves keys.v2.json
// without it, and the copy succeeds once the keychain is back.
func TestCopyLegacyToTeamsWaitsForTheKeychain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	legacyKeys(t, dir)
	unlock := secret.FailForTest(t, dir)
	if err := CopyLegacyToTeams(dir, map[string]string{"acme-platform": "enr-acme"}); err == nil {
		t.Fatal("the copy succeeded without the key")
	}
	unlock()
	if got, _, _ := GetTeam(dir, "enr-acme"); got != "" {
		t.Fatal("a failed copy left a key")
	}
	mustNot(t, CopyLegacyToTeams(dir, map[string]string{"acme-platform": "enr-acme"}))
	if got, _, _ := GetTeam(dir, "enr-acme"); got != testKey {
		t.Fatalf("GetTeam = %q", got)
	}
}

// A crash between the copy and the retirement: the next start copies again, which changes
// nothing, then retires keys.json, whose keys and items go while keys.v2.json's stay.
func TestCrashBetweenCopyAndRetire(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	legacyKeys(t, dir)
	legacy := map[string]string{"acme-platform": "enr-acme"}
	mustNot(t, CopyLegacyToTeams(dir, legacy))
	before := teamsFileText(t, dir)
	// The relay stops here; an older binary still finds its keys.
	if got, _ := Get(dir, "acme-platform"); got != testKey {
		t.Fatal("the copy alone took a key from keys.json")
	}
	mustNot(t, CopyLegacyToTeams(dir, legacy))
	if teamsFileText(t, dir) != before {
		t.Fatal("repeating the copy changed keys.v2.json")
	}
	mustNot(t, RetireV1(dir))
	for _, item := range []string{"key/acme-platform", "key/claude/acme-platform", "key/old-team"} {
		if _, err := secret.Get(dir, item); !errors.Is(err, secret.ErrNotFound) {
			t.Errorf("%s survived the retirement: %v", item, err)
		}
	}
	if f := keysFile(t, dir); strings.Contains(f, "acme-platform") || strings.Contains(f, "old-team") {
		t.Fatalf("keys.json still indexes keys:\n%s", f)
	}
	if got, _ := Get(dir, "acme-platform"); got != "" || len(CollectionProjects(dir)) != 0 {
		t.Fatal("an older binary still finds a key")
	}
	if got, _, err := GetTeam(dir, "enr-acme"); err != nil || got != testKey {
		t.Fatalf("the retirement reached keys.v2.json: %q, %v", got, err)
	}
	mustNot(t, RetireV1(dir))
	mustNot(t, CopyLegacyToTeams(dir, legacy))
	if got, _, _ := GetTeam(dir, "enr-acme"); got != testKey {
		t.Fatal("a copy after the retirement lost the team's key")
	}
	mustNot(t, RetireV1(t.TempDir()))
}

// A keychain that will not remove an item leaves it marked stale and says so; retiring
// again removes it.
func TestRetireV1RetriesTheKeychain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	legacyKeys(t, dir)
	unlock := secret.FailForTest(t, dir)
	if err := RetireV1(dir); err == nil {
		t.Fatal("the retirement hid items the keychain kept")
	}
	if got, _ := Get(dir, "acme-platform"); got != "" {
		t.Fatal("keys.json still indexes a key")
	}
	unlock()
	mustNot(t, RetireV1(dir))
	if _, err := secret.Get(dir, "key/acme-platform"); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("the item survived a second retirement: %v", err)
	}
}
