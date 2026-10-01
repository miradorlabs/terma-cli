package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// In global mode a member may pause only when the organization allows it, and a pause
// recorded before the organization forbade it stops nothing.
func TestPauseHonoursTheOrganization(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(map[bool]string{false: "forbidden", true: "allowed"}[allowed], func(t *testing.T) {
			t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
			if err := config.UpdateProfile(config.DefaultProfile, func(p *config.Profile) {
				p.OrganizationID = "org-test"
				p.Policy = &config.Policy{Mode: config.ModeGlobal, MembersCanPause: allowed, OrganizationID: "org-test",
					TeamID: "team", DefaultProjectID: "team", Revision: 1, FetchedAt: time.Now()}
			}); err != nil {
				t.Fatal(err)
			}
			out, err := runTerma(t, "pause")
			if allowed != (err == nil) {
				t.Fatalf("pause = %v\n%s", err, out)
			}
			if !allowed && !strings.Contains(err.Error(), "does not let members pause") {
				t.Fatalf("the refusal does not say why: %v", err)
			}
			if allowed != capturePaused() {
				t.Fatalf("paused = %v, want %v", capturePaused(), allowed)
			}

			// A pause file left from before the organization changed its mind.
			path, err := config.PausedPath()
			if err != nil {
				t.Fatal(err)
			}
			if err := config.WriteFileAtomic(path, []byte("x\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if allowed != capturePaused() {
				t.Fatalf("a stale pause file: paused = %v, want %v", capturePaused(), allowed)
			}
			if out, err := runTerma(t, "resume"); err != nil || config.Paused() {
				t.Fatalf("resume = %v, still paused = %v\n%s", err, config.Paused(), out)
			}
		})
	}
}
