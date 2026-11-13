package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
)

// On a migrated machine doctor lists the teams, fails while a binding is undecided, and
// shows what Mixed runs dropped: the sessions from the run ledger, the count from the
// relay's counters.
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
	if err := os.WriteFile(filepath.Join(dir, daemon.StatsFile), []byte(`{"counters":{"dropped.mixed.logs":3,"dropped.mixed.spans":2,"dropped.unclaimed.logs":9}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "doctor")
	if err == nil {
		t.Fatalf("doctor passed with a binding undecided:\n%s", out)
	}
	for _, want := range []string{
		"FAIL  teams", "github.com/acme/api is undecided", "terma teams --bind github.com/acme/api=" + orgA().ID,
		"Undecided:", "session " + session, "dropped 5 record(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q:\n%s", want, out)
		}
	}
}
