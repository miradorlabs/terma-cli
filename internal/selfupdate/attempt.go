package selfupdate

import (
	"errors"
	"time"
)

// Attempt is the last install of a release terma tried by itself, which outlives what a look
// found: a release whose install failed is held back a while (held), and one a package
// manager went on to install after the process that started it stopped is recognised by
// the next (landed).
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
	// Interrupted means the process trying it stopped during the install, which holds
	// nothing back; a package manager left to finish may have installed it all the same.
	Interrupted bool `json:"interrupted,omitempty"`
}

// begin records an attempt at release by the build running, at now. Its Retries carry on
// from the attempts at the same release before it, and start again for another.
func (a *Attempt) begin(release, running string, now time.Time) {
	retries := a.Retries
	if a.Attempted != release {
		retries = 0
	}
	*a = Attempt{AttemptAt: now, Attempted: release, AttemptedBy: running, Retries: retries}
}

// end records how the attempt begun last ended. err nil installed it. stopped, its process
// stopping during the install, is not the release's failure, so it is not held back; nor is
// ErrReplaced, another install's binary put in place meanwhile, which forgets the attempt.
// One to try again soon (retrySoon) counts toward its backoff, and any other failure holds
// the release for AttemptInterval.
func (a *Attempt) end(err error, stopped bool) {
	switch {
	case err == nil:
		a.Retries, a.Failure, a.Interrupted = 0, "", false
	case errors.Is(err, ErrReplaced):
		*a = Attempt{}
	case stopped:
		a.Interrupted = true
	case retrySoon(err):
		a.Retries, a.Failure = a.Retries+1, err.Error()
	default:
		a.Retries, a.Failure = 0, err.Error()
	}
}

// held reports whether latest is the release last attempted, whose install failed less
// than its wait ago (Retry), tried by this build (running) or a later one, so that it is not
// tried again yet.
func (a Attempt) held(latest, running string, now time.Time) bool {
	return a.Attempted == latest && !a.Interrupted && now.Before(a.Retry()) && !Newer(a.AttemptedBy, running)
}

// Retry is when the release last attempted may be tried again after its install failed:
// AttemptInterval after it, or retryAfter its Retries for one to try again soon.
func (a Attempt) Retry() time.Time {
	if a.Retries > 0 {
		return a.AttemptAt.Add(retryAfter(a.Retries))
	}
	return a.AttemptAt.Add(AttemptInterval)
}

// landed reports whether the terma running, at current, is the release an earlier build's
// attempt was interrupted installing: its package manager, left to finish, installed it.
func (a Attempt) landed(current string) bool {
	return a.Interrupted && a.Attempted == current && Newer(a.AttemptedBy, current)
}

// retrySoon reports whether err is an attempt to try again soon rather than a day later:
// the package manager does not offer the release yet, or another of its processes is busy.
func retrySoon(err error) bool {
	return errors.Is(err, ErrNotOffered) || errors.Is(err, ErrBusy)
}
