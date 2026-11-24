package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

func seedProfileWithTeam(t *testing.T, dir, stateDir, account string, serverKey bool) {
	t.Helper()
	now := time.Now().UTC()
	p := &config.Profile{
		Team:            "legacy_team",
		Account:         account,
		ServerKeySignIn: serverKey,
		AuthURL:         "https://api.example.com",
		OrganizationID:  "org_legacy",
	}
	if err := config.SaveFile(dir, &config.File{ActiveProfile: "default", Profiles: map[string]*config.Profile{"default": p}}); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	pol := config.Policy{
		Mode: config.ModeRepo, Repositories: []string{"github.com/acme/repo"},
		TeamID: "legacy_team", OrganizationID: "org_legacy", AuthURL: "https://api.example.com",
		Revision: 1, FetchedAt: now,
	}
	if err := config.WriteJSON(filepath.Join(stateDir, "policies", "legacy_team.json"), pol, 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	if !serverKey && account != "" {
		t.Setenv("TERMA_INSECURE_STORAGE", "1")
		if err := saveSignIn(dir, &auth.Credential{
			AuthURL: "https://api.example.com", UserID: account, SessionID: "sess-" + account,
		}); err != nil {
			t.Fatalf("saveSignIn: %v", err)
		}
	}
}

func seedRoutingState(t *testing.T, stateDir, account string) {
	t.Helper()
	now := time.Now().UTC()
	_, err := routing.Create(stateDir, "default", account, func(s *routing.State) error {
		_, err := s.Select(routing.Team{
			OrganizationID: "orgA", AuthURL: "https://api.example.com", TeamID: "team_a",
			Policy: config.Policy{
				Mode: config.ModeRepo, Repositories: []string{"github.com/acme/repo"},
				TeamID: "team_a", OrganizationID: "orgA", AuthURL: "https://api.example.com",
				Revision: 1, FetchedAt: now,
			},
		})
		return err
	})
	if err != nil {
		t.Fatalf("routing.Create: %v", err)
	}
}

func TestHookProfileFailsClosedOnAccountMismatchCorruptAndMidReadRace(t *testing.T) {
	acctA := auth.AccountKey("https://api.example.com", "user-a")
	acctB := auth.AccountKey("https://api.example.com", "user-b")

	t.Run("missing routing state fails closed with zero fallback to Profile.Team", func(t *testing.T) {
		dir, stateDir := t.TempDir(), t.TempDir()
		seedProfileWithTeam(t, dir, stateDir, "", true)
		app := &App{dir: dir, stateDir: stateDir, version: "0.5.0"}
		p := app.hookProfile()
		if p.Routing == nil || p.Routing.Usable() {
			t.Fatalf("Routing = %+v; want unusable fail-closed Routing when no routing state exists", p.Routing)
		}
		if p.Team != "" || p.ServerKey || p.Account != sendas.AccountUnreadable {
			t.Fatalf("missing routing state must fail closed: team=%q serverKey=%v account=%q", p.Team, p.ServerKey, p.Account)
		}
	})

	t.Run("matching routing state activates routing and clears legacy fallback", func(t *testing.T) {
		dir, stateDir := t.TempDir(), t.TempDir()
		seedProfileWithTeam(t, dir, stateDir, "user-a", false)
		seedRoutingState(t, stateDir, acctA)
		app := &App{dir: dir, stateDir: stateDir, version: "0.5.0"}
		p := app.hookProfile()
		if p.Routing == nil || !p.Routing.Usable() {
			t.Fatalf("Routing not usable: %+v", p.Routing)
		}
		if p.Team != "" || p.Account != acctA {
			t.Fatalf("profile = (team=%q account=%q); want empty team and account %q", p.Team, p.Account, acctA)
		}
	})

	t.Run("State.Account != lane fails closed and never falls back to Profile.Team", func(t *testing.T) {
		dir, stateDir := t.TempDir(), t.TempDir()
		seedProfileWithTeam(t, dir, stateDir, "user-a", false)
		seedRoutingState(t, stateDir, acctB) // wrong account!
		app := &App{dir: dir, stateDir: stateDir, version: "0.5.0"}
		p := app.hookProfile()
		if p.Routing == nil || p.Routing.Usable() {
			t.Fatalf("Routing should be non-nil and unusable on account mismatch: %+v", p.Routing)
		}
		if p.Team != "" || p.Policy.Validated() {
			t.Fatalf("fell back to legacy team/policy on account mismatch: team=%q policy=%+v", p.Team, p.Policy)
		}
		if p.Account != sendas.AccountUnreadable {
			t.Fatalf("Account = %q; want %q", p.Account, sendas.AccountUnreadable)
		}
	})

	t.Run("corrupt routing state fails closed and never falls back to Profile.Team", func(t *testing.T) {
		dir, stateDir := t.TempDir(), t.TempDir()
		seedProfileWithTeam(t, dir, stateDir, "", true)
		rpath, err := routing.StatePath(stateDir, "default")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(rpath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(rpath, []byte("{corrupt"), 0o600); err != nil {
			t.Fatal(err)
		}
		app := &App{dir: dir, stateDir: stateDir, version: "0.5.0"}
		p := app.hookProfile()
		if p.Routing == nil || p.Routing.Usable() {
			t.Fatalf("corrupt routing state should fail closed: %+v", p.Routing)
		}
		if p.Team != "" || p.Policy.Validated() {
			t.Fatalf("corrupt routing state fell back to legacy team/policy: team=%q", p.Team)
		}
	})

	t.Run("mid-read mutation of routing state fails closed", func(t *testing.T) {
		dir, stateDir := t.TempDir(), t.TempDir()
		seedProfileWithTeam(t, dir, stateDir, "user-a", false)
		seedRoutingState(t, stateDir, acctA)
		rpath, _ := routing.StatePath(stateDir, "default")
		app := &App{
			dir: dir, stateDir: stateDir, version: "0.5.0",
			betweenProfileReads: func() {
				// Replace routing file mid-snapshot (changing inode/mtime).
				future := time.Now().Add(2 * time.Second)
				_ = os.Chtimes(rpath, future, future)
			},
		}
		p := app.hookProfile()
		if p.Routing == nil || p.Routing.Usable() {
			t.Fatalf("mid-read mutation did not fail closed: %+v", p.Routing)
		}
		if p.Team != "" || p.Account != sendas.AccountUnreadable {
			t.Fatalf("mid-read mutation profile = (team=%q account=%q); want empty team and unreadable", p.Team, p.Account)
		}
	})
}

func TestHookProfileRelayVersionSkewSupersedesAndWithholdsPlacements(t *testing.T) {
	dir, stateDir := t.TempDir(), t.TempDir()
	seedProfileWithTeam(t, dir, stateDir, "", true)
	seedRoutingState(t, stateDir, sendas.NoPersonLane)

	relayDir := claim.Dir(stateDir)
	if err := os.MkdirAll(relayDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Hold relay lock + follow lock (so Supersede writes ReplaceFile without spawning a subprocess).
	unlockRelay, err := flock.TryLock(filepath.Join(relayDir, daemon.LockFile))
	if err != nil {
		t.Fatalf("lock relay: %v", err)
	}
	defer unlockRelay()
	unlockFollow, err := flock.TryLock(filepath.Join(relayDir, daemon.FollowLockFile))
	if err != nil {
		t.Fatalf("lock follow: %v", err)
	}
	defer unlockFollow()

	// Case 1: live pre-routing relay (!RunInfo.Routing) -> Supersede writes ReplaceFile AND WithholdPlacements == true.
	if err := config.WriteJSON(filepath.Join(relayDir, daemon.RunFile), daemon.RunInfo{
		PID: 42424, Version: "0.4.0", Routing: false,
	}, 0o600); err != nil {
		t.Fatalf("write RunFile: %v", err)
	}
	app := &App{dir: dir, stateDir: stateDir, version: "0.5.0"}
	p := app.hookProfile()
	if p.Routing == nil || !p.Routing.Usable() {
		t.Fatalf("Routing should still be usable: %+v", p.Routing)
	}
	if !p.Routing.WithholdPlacements {
		t.Fatal("WithholdPlacements = false for live !RunInfo.Routing relay; want true")
	}
	rep, err := os.ReadFile(filepath.Join(relayDir, daemon.ReplaceFile))
	if err != nil || strings.TrimSpace(string(rep)) != strconv.Itoa(42424) {
		t.Fatalf("ReplaceFile = %q (err=%v); want 42424", string(rep), err)
	}

	// Case 2: live routing-capable relay (RunInfo.Routing == true) -> WithholdPlacements == false.
	if err := config.WriteJSON(filepath.Join(relayDir, daemon.RunFile), daemon.RunInfo{
		PID: 42425, Version: "0.5.0", Routing: true,
	}, 0o600); err != nil {
		t.Fatalf("write RunFile: %v", err)
	}
	p2 := app.hookProfile()
	if p2.Routing == nil || p2.Routing.WithholdPlacements {
		t.Fatalf("WithholdPlacements = true for live RunInfo.Routing relay; want false")
	}
}
