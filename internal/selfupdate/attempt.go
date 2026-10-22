package selfupdate

import (
	"errors"
	"fmt"
	"time"
)

// Attempt is the last install of a release terma tried by itself, which outlives what a look
// found: a release whose install failed is held back a while (held), and one a package
// manager went on to install after the process that started it stopped is recognised by
// the next (landed). Each way an attempt ends is a method of its own.
type Attempt struct {
	AttemptAt time.Time `json:"attempt_at"`
	// Attempted is the release AttemptAt tried to install: another may be tried at once.
	Attempted string `json:"attempted,omitempty"`
	// AttemptedBy is the build that tried it, "" for one from before this was recorded. A
	// later build is not held back by it (held): the failure may have been the earlier
	// build's own, such as a release key it lacks or a fault since fixed.
	AttemptedBy string `json:"attempted_by,omitempty"`
	// Retries counts the attempts at Attempted in a row that are to be tried again soon
	// (ErrNotOffered, ErrBusy), each waiting retryAfter their count, not AttemptInterval.
	Retries int `json:"retries,omitempty"`
	// Failure is why the last attempt failed, for doctor; "" when it did not.
	Failure string `json:"failure,omitempty"`
	// Stopped means the last attempt's process stopped during it, which is not the
	// release's failure: it holds nothing back.
	Stopped bool `json:"stopped,omitempty"`
	// Interrupted means a package manager was left to finish an attempt at Attempted when
	// its process stopped, and may have installed it since (landed). It holds until that
	// release is installed and said, an attempt at another release begins, or the developer
	// updates by hand (byHand).
	Interrupted bool `json:"interrupted,omitempty"`
}

// landWindow is how long after an attempt a release it left installing is taken for its
// own (landed): a package manager left to finish takes minutes, and the relay is back on the
// new release soon after. Later, the developer may have installed it themselves.
const landWindow = time.Hour

// begin records an attempt at release by the build running, at now. Its Retries carry on
// from the attempts at the same release before it, and so does Interrupted, since the
// package manager an earlier one left may still be finishing; another release starts again.
func (a *Attempt) begin(release, running string, now time.Time) {
	retries, interrupted := a.Retries, a.Interrupted
	if a.Attempted != release {
		retries, interrupted = 0, false
	}
	*a = Attempt{AttemptAt: now, Attempted: release, AttemptedBy: running, Retries: retries, Interrupted: interrupted}
}

// installed records that the attempt, or the package manager an earlier one left to
// finish, installed Attempted.
func (a *Attempt) installed() {
	a.Retries, a.Failure, a.Stopped, a.Interrupted = 0, "", false, false
}

// behind records that a package manager a release or more behind installed one before
// Attempted, which is tried again soon.
func (a *Attempt) behind(manager, installed string) {
	a.Retries, a.Failure = a.Retries+1, fmt.Sprintf("%s installed terma %s, not %s yet", manager, installed, a.Attempted)
}

// stopped records that the attempt's process stopped during it, leaving a package manager to
// finish it when leftRunning: not the release's failure, so it is not held back.
func (a *Attempt) stopped(leftRunning bool) {
	a.Stopped = true
	a.Interrupted = a.Interrupted || leftRunning
}

// failed records the attempt's failure, err. ErrReplaced, another install's binary put in
// place meanwhile, forgets the attempt, which another may make at once; one to try again
// soon (retrySoon) counts toward its backoff; any other holds the release AttemptInterval.
func (a *Attempt) failed(err error) {
	switch {
	case errors.Is(err, ErrReplaced):
		*a = Attempt{}
	case retrySoon(err):
		a.Retries, a.Failure = a.Retries+1, err.Error()
	default:
		a.Retries, a.Failure = 0, err.Error()
	}
}

// byHand records that the developer is updating terma by hand (`terma update`): a release an
// interrupted attempt left installing is no longer the background update's to claim.
func (a *Attempt) byHand() {
	a.Interrupted = false
}

// held reports whether latest is the release last attempted, whose install failed less
// than its wait ago (Retry), tried by this build (running) or a later one, so that it is not
// tried again yet; not one whose process stopped during it.
func (a Attempt) held(latest, running string, now time.Time) bool {
	return a.Attempted == latest && !a.Stopped && now.Before(a.Retry()) && !Newer(a.AttemptedBy, running)
}

// Retry is when the release last attempted may be tried again after its install failed:
// AttemptInterval after it, or retryAfter its Retries for one to try again soon.
func (a Attempt) Retry() time.Time {
	if a.Retries > 0 {
		return a.AttemptAt.Add(retryAfter(a.Retries))
	}
	return a.AttemptAt.Add(AttemptInterval)
}

// landed reports whether the terma running, at current, at now, is the release an earlier
// build's attempt left a package manager installing, within landWindow of the attempt.
func (a Attempt) landed(current string, now time.Time) bool {
	return a.Interrupted && a.Attempted == current && Newer(a.AttemptedBy, current) && now.Sub(a.AttemptAt) < landWindow
}

// retrySoon reports whether err is an attempt to try again soon rather than a day later:
// the package manager does not offer the release yet, or another of its processes is busy.
func retrySoon(err error) bool {
	return errors.Is(err, ErrNotOffered) || errors.Is(err, ErrBusy)
}
