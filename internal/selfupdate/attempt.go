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
	// Interrupted is when the attempt began whose package manager was left to finish
	// Attempted as its process stopped, and may have installed it since (landed); zero when
	// none was. It holds until that release is installed and said, an attempt at another
	// release begins, or the developer installs one by hand (byHand).
	Interrupted time.Time `json:"interrupted,omitzero"`
}

// landSlack is how far the file system's clock may stray from the attempt's, either side of
// the time a package manager left to finish could have installed its release (landed).
const landSlack = time.Minute

// begin records an attempt at release by the build running, at now. Its Retries carry on
// from the attempts at the same release before it, and so does Interrupted, since the
// package manager an earlier one left may still be finishing; another release starts again.
func (a *Attempt) begin(release, running string, now time.Time) {
	retries, interrupted := a.Retries, a.Interrupted
	if a.Attempted != release {
		retries, interrupted = 0, time.Time{}
	}
	*a = Attempt{AttemptAt: now, Attempted: release, AttemptedBy: running, Retries: retries, Interrupted: interrupted}
}

// installed records that the attempt, or the package manager an earlier one left to
// finish, installed Attempted.
func (a *Attempt) installed() {
	a.Retries, a.Failure, a.Stopped, a.Interrupted = 0, "", false, time.Time{}
}

// behind records that a package manager a release or more behind installed one before
// Attempted, which is tried again soon.
func (a *Attempt) behind(manager, installed string) {
	a.Retries, a.Failure = a.Retries+1, fmt.Sprintf("%s installed terma %s, not %s yet", manager, installed, a.Attempted)
}

// stopped records that the attempt's process stopped during it, leaving a package manager it
// had started to finish it when leftRunning: not the release's failure, so it is not held
// back. The first attempt that left one running is the one whose release may land.
func (a *Attempt) stopped(leftRunning bool) {
	a.Stopped = true
	if leftRunning && a.Interrupted.IsZero() {
		a.Interrupted = a.AttemptAt
	}
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

// byHand records that the developer installed a release by hand (`terma update`): a release
// an interrupted attempt left installing is no longer the background update's to claim.
func (a *Attempt) byHand() {
	a.Interrupted = time.Time{}
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

// landed reports whether the terma running, at current, put in place at installedAt (as the
// file system has it: Install.installedAt), is the release an earlier build's attempt left a
// package manager installing: put there while that manager could still be running, from
// the attempt's start to UpgradeTimeout after it. Before then, or after, someone else
// installed it, by hand perhaps, and it is not claimed.
func (a Attempt) landed(current string, installedAt time.Time) bool {
	return !a.Interrupted.IsZero() && a.Attempted == current && Newer(a.AttemptedBy, current) &&
		!installedAt.Before(a.Interrupted.Add(-landSlack)) && installedAt.Before(a.Interrupted.Add(UpgradeTimeout+landSlack))
}

// retrySoon reports whether err is an attempt to try again soon rather than a day later:
// the package manager does not offer the release yet, or another of its processes is busy.
func retrySoon(err error) bool {
	return errors.Is(err, ErrNotOffered) || errors.Is(err, ErrBusy)
}
