//go:build !unix

package daemon

import (
	"errors"
	"net"
	"os"
	"time"
)

// Windows has no SCM_RIGHTS, and its supervisor restarts the relay only after it exits: a
// restart leaves the port unbound for about a second.
const handoffSupported = false

func listenerFile(net.Listener) (*os.File, error) { return nil, errors.ErrUnsupported }

func offerListener(string, string, *os.File) (func(time.Duration) bool, error) {
	return nil, errors.ErrUnsupported
}

func take(string, string) net.Listener { return nil }
