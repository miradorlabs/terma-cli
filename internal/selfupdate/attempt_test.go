package selfupdate

import (
	"context"
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
		err     error
		stopped bool
		// retries, failure and interrupted are the attempt after; retry is how long after
		// it its release is held, 0 for not at all.
		retries     int
		failure     string
		interrupted bool
		retry       time.Duration
	}{
		// Held as any attempt is, which no pass asks of a release no longer newer.
		{"installed", Attempt{Attempted: "1.0.1", Retries: 2, Failure: "x"}, "1.0.1", nil, false, 0, "", false, AttemptInterval},
		{"failed", Attempt{}, "1.0.1", errors.New("exit status 1"), false, 0, "exit status 1", false, AttemptInterval},
		{"not offered yet", Attempt{}, "1.0.1", notYet, false, 1, notYet.Error(), false, RetryInterval},
		{"busy, again", Attempt{Attempted: "1.0.1", Retries: 1}, "1.0.1", busy, false, 2, busy.Error(), false, 2 * RetryInterval},
		{"not offered, many times", Attempt{Attempted: "1.0.1", Retries: 9}, "1.0.1", notYet, false, 10, notYet.Error(), false, maxRetryWait},
		{"failed after retries", Attempt{Attempted: "1.0.1", Retries: 3}, "1.0.1", errors.New("EACCES"), false, 0, "EACCES", false, AttemptInterval},
		{"another release starts its own count", Attempt{Attempted: "1.0.1", Retries: 3}, "1.0.2", notYet, false, 1, notYet.Error(), false, RetryInterval},
		{"stopped", Attempt{Attempted: "1.0.1", Retries: 1}, "1.0.1", context.Canceled, true, 1, "", true, 0},
		{"another install's", Attempt{}, "1.0.1", ErrReplaced, true, 0, "", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.before
			a.begin(tc.release, "1.0.0", now)
			a.end(tc.err, tc.stopped)
			if a.Retries != tc.retries || a.Failure != tc.failure || a.Interrupted != tc.interrupted {
				t.Fatalf("after %v: %+v", tc.err, a)
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

// A release an attempt was stopped installing has landed when the terma running is it,
// and an earlier build made the attempt.
func TestAnInterruptedAttemptLandsOnItsRelease(t *testing.T) {
	t.Parallel()
	a := Attempt{Attempted: "1.0.1", AttemptedBy: "1.0.0", Interrupted: true}
	for current, want := range map[string]bool{"1.0.1": true, "1.0.0": false, "1.0.2": false} {
		if got := a.landed(current); got != want {
			t.Errorf("landed(%s) = %v, want %v", current, got, want)
		}
	}
	a.end(nil, false)
	if a.landed("1.0.1") {
		t.Error("an attempt recorded as installed lands again")
	}
}
