package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// upgradedServerKeyMachine is a machine an earlier terma set up with a server key, as this
// one finds it after an update: the profile's team in config.json, the key in keys.json and
// the team's validated policy in policies/, and no routing state. It returns the fake auth
// host, the ingest, the team and its key, with TERMA_API_KEY unset again.
func upgradedServerKeyMachine(t *testing.T) (f *fakeAuth, ingest *recordingIngest, team, key string) {
	t.Helper()
	f = newFakeAuth(t)
	authSandbox(t, f)
	sandboxMachine(t)
	t.Setenv("TERMA_POLICY_STUB", "")
	team = projectsIn(orgA().ID)[0].ID
	key = "ter" + "_srv_0123456789abcdef01234567"
	f.serverKeys = map[string]string{key: team}
	f.policyBody = `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/acme/app"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`
	ingest = newRecordingIngest(t)
	t.Setenv("TERMA_OTLP_URL", ingest.URL)
	t.Setenv("TERMA_API_KEY", key)
	if out, err := runTerma(t, "setup", "-y", "--harness", "claude", "--relay-service", "off"); err != nil {
		t.Fatalf("server-key setup: %v\n%s", err, out)
	}
	t.Setenv("TERMA_API_KEY", "")
	ingest.take()
	// What the earlier terma never wrote.
	if err := os.RemoveAll(filepath.Join(testApp.stateDir, routing.StatesDir)); err != nil {
		t.Fatal(err)
	}
	if hp := testApp.hookProfile(); hp.Routing.Usable() || hp.Account != sendas.AccountUnreadable {
		t.Fatalf("an upgraded server-key machine's hooks before the migration: usable %v, account %q; want failing closed", hp.Routing.Usable(), hp.Account)
	}
	return f, ingest, team, key
}

// An update's refresh turns an upgraded server-key machine's stored team, key and policy into
// the one-team, no-person routing state setup writes, once: hooks then get their lane and
// the spool delivers with the stored key again, with no TERMA_API_KEY and no setup run.
func TestRefreshMigratesAnUpgradedServerKeyProfileToRoutingState(t *testing.T) {
	f, ingest, team, key := upgradedServerKeyMachine(t)
	statePath, err := routing.StatePath(testApp.stateDir, config.DefaultProfile)
	if err != nil {
		t.Fatal(err)
	}

	out, err := runTerma(t, "update", "--refresh")
	if err != nil || !strings.Contains(out, statePath) {
		t.Fatalf("refresh: %v\n%s\nwant it to report %s", err, out, statePath)
	}
	st, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found {
		t.Fatalf("routing.Load after the refresh: found=%v err=%v", found, err)
	}
	if st.Account != sendas.NoPersonLane || len(st.Teams) != 1 {
		t.Fatalf("migrated state: account %q, teams %+v; want one team under %q", st.Account, st.Teams, sendas.NoPersonLane)
	}
	if tm := st.Teams[0]; tm.TeamID != team || tm.OrganizationID != orgA().ID || tm.AuthURL != f.srv.URL || tm.Personal ||
		tm.Policy.TeamID != team || len(tm.Policy.Repositories) != 1 || tm.StateAt(time.Now()) != routing.Active {
		t.Fatalf("migrated team = %+v, want the profile's team with its stored policy, active", tm)
	}
	if b, ok := st.Bindings["github.com/acme/app"]; !ok || b.Refused || b.Organization != orgA().ID {
		t.Fatalf("bindings = %+v, want the policy's repository bound to %s as setup -y binds it", st.Bindings, orgA().ID)
	}

	// Hooks get the no-person lane, and the spool delivers with the stored key alone.
	if hp := testApp.hookProfile(); !hp.Routing.Usable() || !hp.ServerKey || hp.Account != "" {
		t.Fatalf("hooks after the migration: usable %v, server key %v, account %q", hp.Routing.Usable(), hp.ServerKey, hp.Account)
	}
	s := spoolForTest(t)
	appendEvent(t, s, team, time.Now())
	res, err := testApp.flushSpool(t.Context(), true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sent := ingest.take(); res.Sent != 1 || len(sent) != 1 || sent[0].auth != "Bearer "+key || sent[0].project != "" {
		t.Fatalf("delivery after the migration: sent %d, requests %+v; want one with the stored server key and no team header", res.Sent, sent)
	}

	// Once: a second refresh, or a relay starting, writes nothing more.
	if out, err := runTerma(t, "update", "--refresh"); err != nil || strings.Contains(out, statePath) {
		t.Fatalf("second refresh: %v\n%s\nwant nothing written to %s", err, out, statePath)
	}
	if wrote, err := testApp.migrateServerKeyRouting(t.Context()); err != nil || len(wrote) != 0 {
		t.Fatalf("a second migration wrote %v (%v)", wrote, err)
	}
	if again, _, _ := routing.Load(testApp.stateDir, config.DefaultProfile); !routing.SameGeneration(st, again) {
		t.Fatalf("a second migration changed the state: generation %d -> %d", st.Generation, again.Generation)
	}
}

// A relay of this build that starts on an upgraded server-key machine migrates it first: a
// headless machine whose terma a package manager upgraded runs no `terma update`.
func TestRelayStartMigratesAnUpgradedServerKeyProfile(t *testing.T) {
	_, _, team, _ := upgradedServerKeyMachine(t)
	done := make(chan error, 1)
	go func() {
		_, err := within(5*time.Second).combined(t, "relay", "run", "--idle", "0", "--quiet", "--addr", freeAddr(t))
		done <- err
	}()
	awaitRelayRecord(t, claim.Dir(testApp.stateDir), 5*time.Second, done)
	st, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found || st.Account != sendas.NoPersonLane || len(st.Teams) != 1 || st.Teams[0].TeamID != team {
		t.Fatalf("routing state once the relay runs: found=%v err=%v state=%+v", found, err, st)
	}
	<-done
}

// The migration writes only for the profile it can vouch for from this machine's own files:
// anything else stays as it was, failing closed, for `terma setup` to put right.
func TestServerKeyMigrationLeavesWhatItCannotVouchForFailingClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		// spoil makes the machine one the migration must not write a state for.
		spoil func(t *testing.T, team string)
		// state is what routing/<profile>.json must hold afterwards: "" for no file.
		state string
	}{
		{name: "no stored key", spoil: func(t *testing.T, team string) {
			if err := keystore.RetainOnly(testApp.dir, nil); err != nil {
				t.Fatal(err)
			}
			if key, err := keystore.Get(testApp.dir, team); err != nil || key != "" {
				t.Fatalf("the team's key is still on file: %q, %v", key, err)
			}
		}},
		{name: "a person's profile", spoil: func(t *testing.T, _ string) {
			if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.ServerKeySignIn, p.ServerKeyAuthURL = false, "" }); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a key set up against another environment", spoil: func(t *testing.T, _ string) {
			if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.ServerKeyAuthURL = "https://other.example.test" }); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "no team", spoil: func(t *testing.T, _ string) {
			if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.Team = "" }); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "no stored policy", spoil: func(t *testing.T, team string) {
			path, err := config.PolicyPath(testApp.stateDir, team)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a policy fetched for another organization", spoil: func(t *testing.T, team string) {
			path, err := config.PolicyPath(testApp.stateDir, team)
			if err != nil {
				t.Fatal(err)
			}
			pol, ok, err := config.ReadPolicy(testApp.stateDir, team)
			if err != nil || !ok {
				t.Fatalf("stored policy: ok=%v err=%v", ok, err)
			}
			pol.OrganizationID = "org_other"
			if err := config.WriteJSON(path, pol, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "an unreadable policy", spoil: func(t *testing.T, team string) {
			path, err := config.PolicyPath(testApp.stateDir, team)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a corrupt routing state", state: "{corrupt", spoil: func(t *testing.T, _ string) {
			path, err := routing.StatePath(testApp.stateDir, config.DefaultProfile)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, team, _ := upgradedServerKeyMachine(t)
			tc.spoil(t, team)
			wrote, err := testApp.migrateServerKeyRouting(t.Context())
			if err != nil || len(wrote) != 0 {
				t.Fatalf("migration wrote %v (%v), want nothing", wrote, err)
			}
			path, err := routing.StatePath(testApp.stateDir, config.DefaultProfile)
			if err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(path); string(b) != tc.state || (tc.state == "") != os.IsNotExist(err) {
				t.Fatalf("%s holds %q (%v), want %q", path, b, err, tc.state)
			}
			if hp := testApp.hookProfile(); hp.Routing.Usable() {
				t.Fatalf("hooks got a usable routing state the migration could not vouch for")
			}
		})
	}

	// A routing state already there, a person's included, is never replaced.
	t.Run("a routing state of a person's sign-in", func(t *testing.T) {
		f, _, team, _ := upgradedServerKeyMachine(t)
		pol, _, err := config.ReadPolicy(testApp.stateDir, team)
		if err != nil {
			t.Fatal(err)
		}
		const person = "1111111111111111"
		before, err := routing.Create(testApp.stateDir, config.DefaultProfile, person, func(s *routing.State) error {
			_, err := s.Select(routing.Team{OrganizationID: orgA().ID, AuthURL: f.srv.URL, TeamID: team, Policy: pol})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if wrote, err := testApp.migrateServerKeyRouting(t.Context()); err != nil || len(wrote) != 0 {
			t.Fatalf("migration wrote %v (%v) over another lane's state", wrote, err)
		}
		after, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
		if err != nil || !found || after.Account != person || !routing.SameGeneration(before, after) {
			t.Fatalf("the person's routing state after the migration: found=%v err=%v state=%+v", found, err, after)
		}
		if hp := testApp.hookProfile(); hp.Routing.Usable() {
			t.Fatalf("a server-key profile's hooks route by a person's routing state")
		}
	})
}

// Like setup's first routing state, the migration's is never written while a relay of an
// earlier build, which knows nothing of routing, holds the relay's lock: the refresh says
// so and stays due, and the next one migrates.
func TestServerKeyMigrationWaitsForAPreRoutingRelayToStop(t *testing.T) {
	origWait := daemon.GateStopWait
	daemon.GateStopWait = 120 * time.Millisecond
	t.Cleanup(func() { daemon.GateStopWait = origWait })
	upgradedServerKeyMachine(t)
	dir := claim.Dir(testApp.stateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := flock.TryLock(filepath.Join(dir, daemon.LockFile))
	if err != nil {
		t.Fatal(err)
	}
	info, _ := json.Marshal(daemon.RunInfo{PID: os.Getpid(), Version: "0.3.2", Routing: false})
	if err := os.WriteFile(filepath.Join(dir, daemon.RunFile), info, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, run := range []func() error{
		func() error { _, err := testApp.migrateServerKeyRouting(t.Context()); return err },
		func() error { _, err := runTerma(t, "update", "--refresh"); return err },
	} {
		if err := run(); err == nil || !strings.Contains(err.Error(), "pre-routing relay") {
			t.Fatalf("migration beside a pre-routing relay = %v, want it refused", err)
		}
		if _, found, err := routing.Load(testApp.stateDir, config.DefaultProfile); err != nil || found {
			t.Fatalf("a routing state was written beside a pre-routing relay: found=%v err=%v", found, err)
		}
	}
	unlock()
	if err := os.Remove(filepath.Join(dir, daemon.RunFile)); err != nil {
		t.Fatal(err)
	}
	if wrote, err := testApp.migrateServerKeyRouting(t.Context()); err != nil || len(wrote) != 1 {
		t.Fatalf("migration once the pre-routing relay stopped wrote %v (%v)", wrote, err)
	}
}

// A server key's team never collects everything, as setup holds it to: a stored policy in
// global mode migrates to one that lists nothing, until the team lists repositories.
func TestServerKeyMigrationHoldsAGlobalPolicyToItsRepositories(t *testing.T) {
	_, _, team, _ := upgradedServerKeyMachine(t)
	path, err := config.PolicyPath(testApp.stateDir, team)
	if err != nil {
		t.Fatal(err)
	}
	pol, _, err := config.ReadPolicy(testApp.stateDir, team)
	if err != nil {
		t.Fatal(err)
	}
	pol.Mode, pol.Repositories = config.ModeGlobal, nil
	if err := config.WriteJSON(path, pol, 0o600); err != nil {
		t.Fatal(err)
	}
	if wrote, err := testApp.migrateServerKeyRouting(t.Context()); err != nil || len(wrote) != 1 {
		t.Fatalf("migration wrote %v (%v), want the routing state", wrote, err)
	}
	st, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found || len(st.Teams) != 1 || st.Teams[0].Policy.Global() || len(st.Teams[0].Policy.Repositories) != 0 || len(st.Bindings) != 0 {
		t.Fatalf("migrated state: found=%v err=%v state=%+v; want one team that lists nothing", found, err, st)
	}
}
