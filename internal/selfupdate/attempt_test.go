package selfupdate

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// How each attempt ends decides when its release is tried again: a failure a day later,
// one to try again soon after a wait that doubles, a stopped one or another install's at
// once; and an attempt at another release starts its own count.
func TestAnAttemptsEndDecidesItsRetry(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	earlier := now.Add(-5 * time.Minute) // an attempt before, that left its package manager running
	notYet := fmt.Errorf("left at 1.0.0: %w", ErrNotOffered)
	busy := fmt.Errorf("locked: %w", ErrBusy)
	for _, tc := range []struct {
		name    string
		before  Attempt
		release string
		end     func(a *Attempt)
		// after is the attempt's retries, failure, stopped and interrupted after it ends;
		// retry is how long after it its release is held, 0 for not at all.
		retries     int
		failure     string
		stopped     bool
		interrupted bool
		retry       time.Duration
	}{
		// Held as any attempt is, which no pass asks of a release no longer newer.
		{"installed", Attempt{Attempted: "1.0.1", Retries: 2, Failure: "x", Interrupted: earlier}, "1.0.1",
			(*Attempt).installed, 0, "", false, false, AttemptInterval},
		{"failed", Attempt{}, "1.0.1", func(a *Attempt) { a.failed(errors.New("exit status 1")) },
			0, "exit status 1", false, false, AttemptInterval},
		{"not offered yet", Attempt{}, "1.0.1", func(a *Attempt) { a.failed(notYet) },
			1, notYet.Error(), false, false, RetryInterval},
		{"busy, again", Attempt{Attempted: "1.0.1", Retries: 1}, "1.0.1", func(a *Attempt) { a.failed(busy) },
			2, busy.Error(), false, false, 2 * RetryInterval},
		{"not offered, many times", Attempt{Attempted: "1.0.1", Retries: 9}, "1.0.1", func(a *Attempt) { a.failed(notYet) },
			10, notYet.Error(), false, false, maxRetryWait},
		{"failed after retries", Attempt{Attempted: "1.0.1", Retries: 3}, "1.0.1", func(a *Attempt) { a.failed(errors.New("EACCES")) },
			0, "EACCES", false, false, AttemptInterval},
		{"another release starts its own count", Attempt{Attempted: "1.0.1", Retries: 3, Interrupted: earlier}, "1.0.2", func(a *Attempt) { a.failed(notYet) },
			1, notYet.Error(), false, false, RetryInterval},
		{"a tap behind", Attempt{}, "1.0.2", func(a *Attempt) { a.behind("Homebrew", "1.0.1") },
			1, "Homebrew installed terma 1.0.1, not 1.0.2 yet", false, false, RetryInterval},
		{"stopped", Attempt{Attempted: "1.0.1", Retries: 1}, "1.0.1", func(a *Attempt) { a.stopped(false) },
			1, "", true, false, 0},
		{"stopped, its package manager left running", Attempt{}, "1.0.1", func(a *Attempt) { a.stopped(true) },
			0, "", true, true, 0},
		// The retry meets the package manager the stopped attempt left still running, busy:
		// what that one installs is still the background update's.
		{"busy after an interrupted attempt", Attempt{Attempted: "1.0.1", Interrupted: earlier, Stopped: true}, "1.0.1", func(a *Attempt) { a.failed(busy) },
			1, busy.Error(), false, true, RetryInterval},
		{"another install's", Attempt{Interrupted: earlier}, "1.0.1", func(a *Attempt) { a.failed(ErrReplaced) },
			0, "", false, false, 0},
		{"updated by hand", Attempt{Attempted: "1.0.1", Interrupted: earlier}, "1.0.1", func(a *Attempt) { a.stopped(false); a.byHand() },
			0, "", true, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.before
			a.begin(tc.release, "1.0.0", now)
			tc.end(&a)
			if a.Retries != tc.retries || a.Failure != tc.failure || a.Stopped != tc.stopped || a.Interrupted.IsZero() == tc.interrupted {
				t.Fatalf("after it: %+v", a)
			}
			for _, at := range []time.Duration{0, tc.retry - time.Second, tc.retry + time.Second} {
				want := tc.retry > 0 && at < tc.retry
				if got := a.held(tc.release, "1.0.0", now.Add(at)); got != want {
					t.Errorf("held %s after: %v, want %v", at, got, want)
				}
			}
			// A later build is never held back by an earlier one's attempt.
			if a.held(tc.release, "1.0.1", now) {
				t.Error("a later build is held back")
			}
		})
	}
}

// A release an attempt left a package manager installing has landed when the terma running
// is it, an earlier build made the attempt, and the file system says it was put in place
// while that manager could have been running: from the attempt to UpgradeTimeout after it.
// Before or after, someone else installed it, by hand perhaps.
func TestAnInterruptedAttemptLandsOnItsRelease(t *testing.T) {
	t.Parallel()
	began := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	a := Attempt{AttemptAt: began, Attempted: "1.0.1", AttemptedBy: "1.0.0"}
	a.stopped(true)
	if a.Interrupted != began {
		t.Fatalf("a stop that left the manager running recorded %v", a.Interrupted)
	}
	for _, tc := range []struct {
		name, current string
		installedAt   time.Time
		want          bool
	}{
		{"during the attempt", "1.0.1", began.Add(3 * time.Minute), true},
		{"as the manager's time ran out", "1.0.1", began.Add(UpgradeTimeout), true},
		{"another release", "1.0.2", began.Add(3 * time.Minute), false},
		{"the release before", "1.0.0", began.Add(3 * time.Minute), false},
		{"before the attempt", "1.0.1", began.Add(-time.Hour), false},
		{"long after", "1.0.1", began.Add(UpgradeTimeout + time.Hour), false},
		{"unknown", "1.0.1", time.Time{}, false},
	} {
		if got := a.landed(tc.current, tc.installedAt); got != tc.want {
			t.Errorf("%s: landed = %v, want %v", tc.name, got, tc.want)
		}
	}
	// A retry that begins later, and finds the manager busy, keeps the first attempt's time.
	a.begin("1.0.1", "1.0.0", began.Add(20*time.Minute))
	a.failed(fmt.Errorf("locked: %w", ErrBusy))
	if !a.landed("1.0.1", began.Add(3*time.Minute)) {
		t.Error("a retry lost the claim of the manager the first attempt left")
	}
	// A stop that left nothing running claims nothing.
	idle := Attempt{AttemptAt: began, Attempted: "1.0.1", AttemptedBy: "1.0.0"}
	idle.stopped(false)
	if idle.landed("1.0.1", began.Add(time.Minute)) {
		t.Error("a stop that started no manager claims a release")
	}
	b := a
	b.byHand()
	if b.landed("1.0.1", began.Add(3*time.Minute)) {
		t.Error("an install by hand is taken for the background's")
	}
	a.installed()
	if a.landed("1.0.1", began.Add(3*time.Minute)) {
		t.Error("an attempt recorded as installed lands again")
	}
}
