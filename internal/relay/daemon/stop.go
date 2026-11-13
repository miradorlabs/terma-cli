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
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// stopReason is why a relay stops.
type stopReason int

const (
	// stopAsked is a signal, or Stop's request where there are no signals (Windows).
	stopAsked stopReason = iota
	stopIdle
	stopReplaced
	stopUpdated
	// stopUpdatedByAnother is stopUpdated for a release another install put in place.
	stopUpdatedByAnother
	stopSetupGone
	stopServeFailed
	// stopUnfinished: the migration stopped part way (Config.migrate).
	stopUnfinished
)

// describe names how a run ended, for the relay log.
func (r stopReason) describe(err error) string {
	switch {
	case err != nil:
		return err.Error()
	case r == stopReplaced:
		return "a newer terma asked it to make way"
	case r == stopUpdated:
		return "it installed a newer terma"
	case r == stopUpdatedByAnother:
		return "another install replaced its terma"
	case r == stopSetupGone:
		return "its setup was removed"
	case r == stopIdle:
		return "idle"
	}
	return "asked to stop"
}

// exitBeatWait bounds the last heartbeat, which a stopped relay's process waits for.
const exitBeatWait = 3 * time.Second

// exit is the last heartbeat's terma.relay.exit.reason for this run's end.
func (r stopReason) exit(err error) string {
	switch {
	case err != nil:
		return semconv.TermaRelayExitReasonServeFailed
	case r == stopReplaced:
		return semconv.TermaRelayExitReasonReplaced
	case r == stopUpdated, r == stopUpdatedByAnother:
		return semconv.TermaRelayExitReasonUpdated
	case r == stopIdle:
		return semconv.TermaRelayExitReasonIdle
	}
	return semconv.TermaRelayExitReasonAsked
}

// watch waits for a reason to stop. A relay migrating the machine (migrated, until it
// says how that went) is never idle, and stops with the migration's error when it stopped
// part way. A relay that installed a newer terma stops once Quiesce
// finds its hold empty, no export in flight and none for updateQuiet (for none at all after
// updatePauseWait), however long that takes: the release is already in place for every
// hook, so waiting loses nothing, while what it holds in memory would be lost. Its queue is
// on disk, so a route still retrying never keeps it, and a newer hook asking it to make way
// is answered by that same wait.
func watch(ctx context.Context, r *relay.Relay, stateDir string, idle time.Duration, served <-chan error, updated <-chan Pass, migrated <-chan error) (stopReason, string, error) {
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
		case err := <-migrated:
			if err != nil {
				return stopUnfinished, installed.Installed, err
			}
			migrated = nil
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
		} else if !replacing.IsZero() && (!r.Holding() || time.Since(replacing) >= replacedMaxWait) {
			// Once the hold is empty, since what it keeps is lost at exit; the queue is on disk.
			return stopReplaced, installed.Installed, nil
		}
		if time.Since(lastPrune) > time.Hour {
			claim.Prune(stateDir, time.Now())
			pruneRuns(stateDir, r, time.Now())
			lastPrune = time.Now()
		}
		if d, quiet := r.Idle(); quiet && idle > 0 && d >= idle && migrated == nil {
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
