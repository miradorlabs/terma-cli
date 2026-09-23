package cmd

import (
	"errors"
	"strconv"
)

// Exit codes for commands whose outcome is not a plain success/failure. The
// default for an error is 1 (see Execute); these name the cases a script needs to
// tell apart, and `terma spool flush` is the first command to use them.
//
// 2 is deliberately not 1: a flush that declined to run has delivered nothing,
// which is a normal state on a machine whose backend is unreachable, not the
// failure-to-run that 1 means.
const (
	// ExitBackoff: the command declined to run because a previous attempt failed
	// and its retry window is still open. An explicit --force overrides it.
	ExitBackoff = 2
	// ExitIncomplete: the command ran and did not fail, but some work was left
	// undone — for a flush, events still queued or given up on.
	ExitIncomplete = 3
)

// exitError carries an exit code out of a command without printing an error
// message: the command has already explained itself on stdout. Execute unwraps it
// rather than reporting it as a failure.
type exitError struct{ code int }

func (e exitError) Error() string { return "exit code " + strconv.Itoa(e.code) }

// exitWith ends a command with a specific exit code and no error text.
func exitWith(code int) error { return exitError{code: code} }

// exitCodeOf reports the code an error asks for, and whether it asked for one.
func exitCodeOf(err error) (int, bool) {
	if e, ok := errors.AsType[exitError](err); ok {
		return e.code, true
	}
	return 0, false
}
