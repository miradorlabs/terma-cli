package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/migrate"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// legacyPersonalMachine is a machine before multi-org routing, signed in to Beta Labs,
// which the fake auth host lists as the developer's personal organization, collecting
// everything with its one team; the agents' hooks files sit in the test's own folders.
func legacyPersonalMachine(t *testing.T) *fakeAuth {
	t.Helper()
	f := newFakeAuth(t)
	authSandbox(t, f)
	sandboxMachine(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("COPILOT_HOME", t.TempDir())
	f.orgs = []organization{{ID: orgA().ID, Name: "Acme", Type: "shared"}, {ID: orgB().ID, Name: "Beta Labs", Type: "personal"}}
	team := betaCore
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) {
		p.OrganizationID, p.OrganizationName, p.Team, p.TeamName = orgB().ID, orgB().Name, team, "Core"
	}); err != nil {
		t.Fatal(err)
	}
	if err := config.WritePolicy(testApp.stateDir, config.Policy{Mode: config.ModeGlobal, GitHooks: true, Revision: 2,
		TeamID: team, OrganizationID: orgB().ID, AuthURL: f.srv.URL, FetchedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f, orgB())); err != nil {
		t.Fatal(err)
	}
	if err := keystore.Set(testApp.dir, team, testServerKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	return f
}

// hookWith writes Claude's machine-wide hooks running the terma at path, as setup does.
func hookWith(t *testing.T, path string) {
	t.Helper()
	settings := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json")
	body := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":` + jsonString(hookmgr.UserHookCommand(path)("stop")) + `}]}]}}`
	if err := os.WriteFile(settings, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func jsonString(s string) string {
	b, _ := hookmgr.MarshalJSON(s, "", "")
	return string(b)
}

// fakeInstall writes a stand-in terma binary of build at a new path.
func fakeInstall(t *testing.T, build string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "terma")
	if err := os.WriteFile(path, []byte(build), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// The relay migrates with what the command line knows: which terma the agents' hooks run,
// read from their hooks files, and each team's organization type, asked with its
// profile's own sign-in. A personal organization's team migrates as personal, keeping its
// global policy and binding nothing.
func TestTheRelaysMigrationAsksTheHooksAndTheOrganizationType(t *testing.T) {
	f := legacyPersonalMachine(t)
	exe := fakeInstall(t, "build 2")
	hookWith(t, exe)

	hooked, err := testApp.hookedTermas()
	if err != nil || !slices.Equal(hooked, []string{exe}) {
		t.Fatalf("hookedTermas = %q, %v", hooked, err)
	}
	if kind, err := testApp.organizationType(context.Background(), config.DefaultProfile, orgB().ID); err != nil || kind != migrate.OrgPersonal {
		t.Fatalf("organizationType = %q, %v", kind, err)
	}
	m := testApp.relayMigration(exe)
	m.FlushWait = time.Second
	if out, err := migrate.Run(context.Background(), *m); err != nil || out != migrate.Committed {
		t.Fatalf("migrate = %s, %v", out, err)
	}
	if f.orgLists.Load() == 0 {
		t.Fatal("the organization type was never asked")
	}
	st, ok, err := routing.LoadReady(testApp.stateDir, config.DefaultProfile)
	if err != nil || !ok || len(st.Teams) != 1 {
		t.Fatalf("routing state = %+v, %v, %v", st, ok, err)
	}
	if tm := st.Teams[0]; !tm.Personal || !tm.Policy.Global() || tm.TeamID != betaCore || len(st.Bindings) != 0 {
		t.Fatalf("the personal team became %+v, bindings %v", tm, st.Bindings)
	}
	if key, _, err := keystore.GetTeam(testApp.dir, st.Teams[0].EnrollmentID); err != nil || key != testServerKey {
		t.Fatalf("its key in keys.v2.json = %q, %v", key, err)
	}
}

// Hooks that run another terma than the relay's keep the machine as it was, and say which.
func TestTheRelaysMigrationWaitsForOneInstall(t *testing.T) {
	legacyPersonalMachine(t)
	exe, older := fakeInstall(t, "build 2"), fakeInstall(t, "build 1")
	hookWith(t, older)
	m := testApp.relayMigration(exe)
	m.FlushWait = time.Second
	if out, err := migrate.Run(context.Background(), *m); err != nil || out != migrate.NotMigrated || config.Migrated(testApp.dir) {
		t.Fatalf("migrate = %s, %v", out, err)
	}
	if b, ok := migrate.ReadBlock(testApp.stateDir); !ok || b.Reason != migrate.TwoInstalls || !slices.Equal(b.Others, []string{older}) {
		t.Fatalf("block = %+v, %v", b, ok)
	}
}

// A sign-in its organization refuses leaves the type unknown: the machine keeps today's
// rules until a relay start can ask.
func TestTheRelaysMigrationWaitsForTheOrganizationType(t *testing.T) {
	f := legacyPersonalMachine(t)
	f.deadToken = "ter_cli_" + orgB().ID
	exe := fakeInstall(t, "build 2")
	hookWith(t, exe)
	m := testApp.relayMigration(exe)
	m.FlushWait = time.Second
	if out, err := migrate.Run(context.Background(), *m); err != nil || out != migrate.NotMigrated || config.Migrated(testApp.dir) {
		t.Fatalf("migrate = %s, %v", out, err)
	}
	if b, ok := migrate.ReadBlock(testApp.stateDir); !ok || b.Reason != migrate.OrgTypeUnknown {
		t.Fatalf("block = %+v, %v", b, ok)
	}
	if key, err := keystore.Get(testApp.dir, betaCore); err != nil || key != testServerKey {
		t.Fatalf("keys.json lost its key: %q, %v", key, err)
	}
}
