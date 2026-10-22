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
		{"installed", Attempt{Attempted: "1.0.1", Retries: 2, Failure: "x", Interrupted: true}, "1.0.1",
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
		{"another release starts its own count", Attempt{Attempted: "1.0.1", Retries: 3, Interrupted: true}, "1.0.2", func(a *Attempt) { a.failed(notYet) },
			1, notYet.Error(), false, false, RetryInterval},
		{"a tap behind", Attempt{}, "1.0.2", func(a *Attempt) { a.behind("Homebrew", "1.0.1") },
			1, "Homebrew installed terma 1.0.1, not 1.0.2 yet", false, false, RetryInterval},
		{"stopped", Attempt{Attempted: "1.0.1", Retries: 1}, "1.0.1", func(a *Attempt) { a.stopped(false) },
			1, "", true, false, 0},
		{"stopped, its package manager left running", Attempt{}, "1.0.1", func(a *Attempt) { a.stopped(true) },
			0, "", true, true, 0},
		// The retry meets the package manager the stopped attempt left still running, busy:
		// what that one installs is still the background update's.
		{"busy after an interrupted attempt", Attempt{Attempted: "1.0.1", Interrupted: true, Stopped: true}, "1.0.1", func(a *Attempt) { a.failed(busy) },
			1, busy.Error(), false, true, RetryInterval},
		{"another install's", Attempt{Interrupted: true}, "1.0.1", func(a *Attempt) { a.failed(ErrReplaced) },
			0, "", false, false, 0},
		{"updated by hand", Attempt{Attempted: "1.0.1", Interrupted: true}, "1.0.1", func(a *Attempt) { a.stopped(false); a.byHand() },
			0, "", true, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.before
			a.begin(tc.release, "1.0.0", now)
			tc.end(&a)
			if a.Retries != tc.retries || a.Failure != tc.failure || a.Stopped != tc.stopped || a.Interrupted != tc.interrupted {
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
// is it, an earlier build made the attempt, and it is soon after: later, the developer may
// have installed it by hand.
func TestAnInterruptedAttemptLandsOnItsRelease(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	a := Attempt{AttemptAt: now, Attempted: "1.0.1", AttemptedBy: "1.0.0", Interrupted: true}
	for current, want := range map[string]bool{"1.0.1": true, "1.0.0": false, "1.0.2": false} {
		if got := a.landed(current, now.Add(time.Minute)); got != want {
			t.Errorf("landed(%s) = %v, want %v", current, got, want)
		}
	}
	if a.landed("1.0.1", now.Add(landWindow+time.Minute)) {
		t.Error("a release that landed long after the attempt is taken for its own")
	}
	b := a
	b.byHand()
	if b.landed("1.0.1", now.Add(time.Minute)) {
		t.Error("an update by hand is taken for the background's")
	}
	a.installed()
	if a.landed("1.0.1", now.Add(time.Minute)) {
		t.Error("an attempt recorded as installed lands again")
	}
}
