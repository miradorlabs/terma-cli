package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/migrate"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
)

// migratedForTeardown is a machine the relay migrated: one profile signed in to acme, whose
// sign-in environment answers nothing, with team acme-platform under one enrollment, and
// a relay set up. It returns the enrollment.
func migratedForTeardown(t *testing.T, configDir, stateDir string) string {
	t.Helper()
	authURL := "http://" + freeAddr(t)
	if err := config.UpdateProfile(configDir, "default", func(p *config.Profile) {
		p.OrganizationID, p.Team, p.AuthURL = "acme", "acme-platform", authURL
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SaveCredential(configDir, "default", &auth.Credential{AccessToken: "ter_cli_a", RefreshToken: "ter_clr_a",
		ExpiresAt: time.Now().Add(time.Hour), SessionID: "s", OrganizationID: "acme", AuthURL: authURL}); err != nil {
		t.Fatal(err)
	}
	if err := keystore.Set(configDir, "acme-platform", testServerKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	if out, err := migrate.Run(context.Background(), migrate.Machine{ConfigDir: configDir, StateDir: stateDir, FlushWait: time.Second,
		OrgType: func(context.Context, string, string) (string, error) { return migrate.OrgShared, nil }}); err != nil || out != migrate.Committed {
		t.Fatalf("migrate = %s, %v", out, err)
	}
	s, ok, err := routing.Load(stateDir, "default")
	if !ok || err != nil || len(s.Teams) != 1 {
		t.Fatalf("routing state = %+v, %v", s, err)
	}
	relayDir := filepath.Join(stateDir, claim.DirName)
	if err := os.MkdirAll(relayDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(relayDir, "token"), []byte("t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return s.Teams[0].EnrollmentID
}

// On a migrated machine teardown keeps what the developer chose and signed in with: the
// routing state, the team's key in keys.v2.json and credentials.v2.json, so the machine
// stays migrated, even after --sign-out, and the next setup never starts afresh over its
// bindings. A run Mixed before teardown stays Mixed; only expired ledgers go, with the
// record of a migration that did not happen, the team names `terma teams` listed and a
// routing state setup set aside as unreadable.
func TestTeardownKeepsAMigratedMachineMigrated(t *testing.T) {
	_, configDir, stateDir, _ := nateTestEnvironment(t)
	enrollment := migratedForTeardown(t, configDir, stateDir)
	statePath, err := routing.StatePath(stateDir, "default")
	if err != nil {
		t.Fatal(err)
	}
	stateBefore, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	mixedAt := time.Now().Add(-time.Minute)
	if _, err := runs.Start(stateDir, "live-session", mixedAt.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := runs.MarkMixed(stateDir, "live-session", mixedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := runs.Start(stateDir, "old-session", time.Now().Add(-20*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(stateDir, runs.Dir, "old-session.json")
	expired := time.Now().Add(-runs.Retention - time.Hour)
	if err := os.Chtimes(old, expired, expired); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(stateDir, migrate.BlockFile)
	if err := os.WriteFile(blocked, []byte(`{"reason":"flush_busy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	names, setAside := teamNamesPath(stateDir, "default"), statePath+".unreadable"
	if err := os.MkdirAll(filepath.Dir(names), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{names, setAside} {
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, args := range [][]string{{"teardown", "--yes"}, {"teardown", "--yes", "--sign-out"}} {
		if out, err := runTerma(t, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		if !config.Migrated(configDir) {
			t.Fatalf("%v unmarked the machine migrated", args)
		}
		if after, err := os.ReadFile(statePath); err != nil || string(after) != string(stateBefore) {
			t.Fatalf("%v changed the routing state: %v", args, err)
		}
		if key, _, err := keystore.GetTeam(configDir, enrollment); err != nil || key != testServerKey {
			t.Fatalf("%v: the team's key = %q, %v", args, key, err)
		}
		if at, mixed := runs.MixedAt(stateDir, "live-session", time.Now()); !mixed || !at.Equal(mixedAt.UTC()) && !at.Equal(mixedAt) {
			t.Fatalf("%v forgot a Mixed run: %v, %v", args, at, mixed)
		}
		if _, err := os.Stat(old); !os.IsNotExist(err) {
			t.Fatalf("%v kept an expired ledger: %v", args, err)
		}
		for _, path := range []string{blocked, names, setAside} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("%v kept %s: %v", args, path, err)
			}
		}
	}
	if creds, err := auth.Credentials(configDir, "default"); err != nil || len(creds) != 0 {
		t.Fatalf("--sign-out left %d sign-ins: %v", len(creds), err)
	}
}
