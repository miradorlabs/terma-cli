package routing

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// On a migrated machine StorePolicy keeps each team's policy file and never writes the
// profile's team, whatever organization the team is in; a revision never moves back.
func TestStorePolicyNeverWritesTheProfilesTeamOnceMigrated(t *testing.T) {
	cfg := &config.Config{Dir: t.TempDir(), StateDir: t.TempDir(), ProfileName: config.DefaultProfile, OrganizationID: "org_a", AuthURL: "https://auth.example"}
	if err := config.UpdateProfile(cfg.Dir, cfg.ProfileName, func(p *config.Profile) { p.OrganizationID = "org_a" }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.CredentialsV2Path(cfg.Dir), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err := os.ReadFile(config.Path(cfg.Dir))
	if err != nil {
		t.Fatal(err)
	}
	pol := config.Policy{Mode: config.ModeRepo, TeamID: "team-b", OrganizationID: "org_b", AuthURL: "https://auth.example", Revision: 4, FetchedAt: time.Now()}
	if err := StorePolicy(cfg, &pol); err != nil {
		t.Fatalf("another organization's team: %v", err)
	}
	if stored, ok, err := config.ReadPolicy(cfg.StateDir, "team-b"); err != nil || !ok || stored.Revision != 4 {
		t.Fatalf("stored %+v, %v, %v", stored, ok, err)
	}
	if after, _ := os.ReadFile(config.Path(cfg.Dir)); !bytes.Equal(after, settings) {
		t.Fatalf("config.json changed:\n%s", after)
	}
	older := pol
	older.Revision = 3
	if err := StorePolicy(cfg, &older); err == nil {
		t.Fatal("stored an older revision")
	}
	unscoped := pol
	unscoped.OrganizationID = ""
	if err := StorePolicy(cfg, &unscoped); err == nil {
		t.Fatal("stored a policy no organization was named for")
	}
}

// Before migration StorePolicy is as it was: it adopts the fetched team as the profile's.
func TestStorePolicyBeforeMigrationStillSelectsTheTeam(t *testing.T) {
	cfg := &config.Config{Dir: t.TempDir(), StateDir: t.TempDir(), ProfileName: config.DefaultProfile, OrganizationID: "org_a", AuthURL: "https://auth.example"}
	pol := config.Policy{Mode: config.ModeRepo, TeamID: "team-a", OrganizationID: "org_a", AuthURL: "https://auth.example", Revision: 1, FetchedAt: time.Now()}
	if err := StorePolicy(cfg, &pol); err != nil {
		t.Fatal(err)
	}
	file, err := config.LoadFile(cfg.Dir)
	if err != nil || file.Profiles[cfg.ProfileName] == nil || file.Profiles[cfg.ProfileName].Team != "team-a" {
		t.Fatalf("profile = %+v, %v", file, err)
	}
}
