package cli

import (
	"errors"
	"strconv"
)

// Exit codes a script needs to tell apart from an error's 1: a declined flush is a normal
// state on a machine whose backend is unreachable, not a failure to run.
const (
	// ExitBackoff means the command declined to run because a retry window is open.
	ExitBackoff = 2
	// ExitIncomplete means the command ran and did not fail, but left work undone.
	ExitIncomplete = 3
	// ExitUploadRunning: `terma history upload --json-stream` found another upload of this
	// profile running.
	ExitUploadRunning = 4
	// ExitNotSetUp: `terma history upload --json-stream` found no `terma setup` for the
	// signed-in account.
	ExitNotSetUp = 5
	// ExitSignedOut: `terma history upload --json-stream` found no sign-in, or one the server
	// retired.
	ExitSignedOut = 6
	// ExitRestart asks the service manager to start a newer binary (EX_TEMPFAIL).
	ExitRestart = 75
)

// exitError carries a code without error text: the command has already explained itself.
type exitError struct{ code int }

func (e exitError) Error() string { return "exit code " + strconv.Itoa(e.code) }

func exitWith(code int) error { return exitError{code: code} }

func exitCodeOf(err error) (int, bool) {
	if e, ok := errors.AsType[exitError](err); ok {
		return e.code, true
	}
	return 0, false
}
