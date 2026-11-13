package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
)

// On a migrated machine doctor lists the teams, fails while a binding is undecided, and
// shows what Mixed runs dropped and what each blocker held: the sessions from the run
// ledger, the counts from the relay's stats.json blocked keys.
func TestDoctorOnAMigratedMachineShowsTheTeamsAndFailsOnWhatIsPending(t *testing.T) {
	migratedMachine(t, func(f *fakeAuth, s *routing.State) error {
		_, err := s.Select(stateTeam(f, orgA().ID, acmeAPI, false, "github.com/acme/api"))
		return err
	}, orgA())
	const session = "0f5c2b8e-3d7a-4c1e-9b6f-2a8d4e7c1b3a"
	if _, err := runs.Start(testApp.stateDir, session, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := runs.MarkMixed(testApp.stateDir, session, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	dir := claim.Dir(testApp.stateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The relay's last stop recorded what it kept from leaving, by what blocked it.
	stats, err := json.Marshal(relay.Snapshot{Counters: map[string]int{"dropped.mixed_run.logs": 5, "dropped.unclaimed.logs": 9},
		Blocked: map[string]int{"mixed:" + session: 3, "mixed:another-session": 2, "pending:github.com/acme/api": 4}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, daemon.StatsFile), stats, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "doctor")
	if err == nil {
		t.Fatalf("doctor passed with a binding undecided:\n%s", out)
	}
	for _, want := range []string{
		"FAIL  teams", "github.com/acme/api is undecided", "terma teams --bind github.com/acme/api=" + orgA().ID,
		"Undecided:", "session " + session, "dropped 5 record(s)", "4 record(s) for github.com/acme/api, undecided",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q:\n%s", want, out)
		}
	}
}
