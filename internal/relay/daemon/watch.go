package daemon

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// watch waits for a reason to stop. A relay that installed a newer terma stops once Quiesce
// finds its hold empty, no export in flight and none for updateQuiet (for none at all after
// updatePauseWait), however long that takes: the release is already in place for every
// hook, so waiting loses nothing, while what it holds in memory would be lost. Its queue is
// on disk, so a route still retrying never keeps it, and a newer hook asking it to make way
// is answered by that same wait. Every way out but a signal, a teardown or a cap asks the
// work beside the engine to halt last (TryHalt), and waits while an image is on its way.
func watch(ctx context.Context, r *relay.Relay, stateDir string, idle time.Duration, served <-chan error, updated <-chan Pass) (stopReason, string, error) {
	dir := claim.Dir(stateDir)
	var replacing time.Time // when a newer terma asked this relay to make way
	var upgraded time.Time  // when it installed one itself
	var installed Pass      // the release it installed last, or found in place
	lastPrune := time.Now()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return stopAsked, installed.Installed, nil
		case err := <-served:
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			return stopServeFailed, installed.Installed, err
		case installed = <-updated:
			// From the first release it installed: one installed while it waits to restart is
			// no reason to keep running the release they both replaced any longer.
			if upgraded.IsZero() {
				upgraded = time.Now()
			}
		case <-tick.C:
		}
		if !setUp(stateDir) {
			return stopSetupGone, installed.Installed, nil
		}
		if requested(dir, StopFile) {
			return stopAsked, installed.Installed, nil
		}
		if replacing.IsZero() && requested(dir, ReplaceFile) {
			replacing = time.Now()
		}
		if !upgraded.IsZero() {
			quiet := updateQuiet
			if time.Since(upgraded) >= updatePauseWait {
				quiet = 0
			}
			if r.Quiesce(quiet) {
				if installed.Replaced {
					return stopUpdatedByAnother, installed.Installed, nil
				}
				return stopUpdated, installed.Installed, nil
			}
		} else if !replacing.IsZero() && (!r.Holding() && r.TryHalt() || time.Since(replacing) >= replacedMaxWait) {
			// Once the hold is empty, since what it keeps is lost at exit, and the image sender
			// halts between two images: one cut off would be sent again whole. The queues are
			// on disk.
			return stopReplaced, installed.Installed, nil
		}
		if time.Since(lastPrune) > time.Hour {
			claim.Prune(stateDir, time.Now())
			lastPrune = time.Now()
		}
		if d, quiet := r.Idle(); quiet && idle > 0 && d >= idle && r.TryHalt() {
			return stopIdle, installed.Installed, nil
		}
	}
}

// requested takes the request in dir's file name; one naming another pid is a dead
// relay's, and is cleared.
func requested(dir, name string) bool {
	path := filepath.Join(dir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	_ = os.Remove(path)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid == os.Getpid()
}
