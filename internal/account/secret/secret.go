// Package secret keeps terma's secrets in the system's credential store: the Keychain on
// macOS, the Credential Manager on Windows, the Secret Service on Linux. Each config
// directory has a service of its own, so separate installs never share an item.
package secret

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/zalando/go-keyring"
)

// ErrNotFound reports that the store has no such item.
var ErrNotFound = errors.New("secret not found")

// UnavailableError reports a store that could not be reached, was locked or timed out:
// the secret may well exist, so callers never read it as absent.
type UnavailableError struct {
	Err error
	// TimedOut is a call given up on while it still ran: a write may yet land.
	TimedOut bool
}

func (e *UnavailableError) Error() string { return "system keychain unavailable: " + e.Err.Error() }

func (e *UnavailableError) Unwrap() error { return e.Err }

// IsUnavailable reports whether err is, or wraps, an UnavailableError.
func IsUnavailable(err error) bool {
	var u *UnavailableError
	return errors.As(err, &u)
}

// MayLand reports whether err is a write given up on while it still ran, which may yet
// store its value.
func MayLand(err error) bool {
	var u *UnavailableError
	return errors.As(err, &u) && u.TimedOut
}

// A read answers an export or a flush, so it gives up soon; a write may wait for someone
// typing a keychain password, as gh does.
const (
	readTimeout  = 5 * time.Second
	writeTimeout = time.Minute
)

type store interface {
	Set(service, user, password string) error
	Get(service, user string) (string, error)
	Delete(service, user string) error
}

type systemStore struct{}

func (systemStore) Set(service, user, password string) error {
	return keyring.Set(service, user, password)
}
func (systemStore) Get(service, user string) (string, error) { return keyring.Get(service, user) }
func (systemStore) Delete(service, user string) error        { return keyring.Delete(service, user) }

// Service names the keychain service that holds the config directory dir's items, as
// Keychain Access and the other stores show it.
func Service(dir string) string { return "terma:" + encode(dir) }

// encode keeps a name to characters every backend passes through as they are: macOS's
// writes the name into a `security -i` command line, which reads quotes and newlines in
// its own way. Anything else is percent-encoded, '%' included, so no two names collide.
func encode(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("-._~/:@", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// Get returns the secret name holds for the config directory dir.
func Get(dir, name string) (string, error) {
	return bounded(readTimeout, dir, name, func(s store, service, user string) (string, error) {
		return s.Get(service, user)
	})
}

// Set stores value as name for the config directory dir, replacing any earlier value.
func Set(dir, name, value string) error {
	_, err := bounded(writeTimeout, dir, name, func(s store, service, user string) (string, error) {
		return "", s.Set(service, user, value)
	})
	return err
}

// Delete removes name for the config directory dir; a missing item is not an error.
func Delete(dir, name string) error {
	_, err := bounded(readTimeout, dir, name, func(s store, service, user string) (string, error) {
		return "", s.Delete(service, user)
	})
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// inFlight bounds the backend calls running at once. A call that never returns keeps its
// slot, so a wedged store holds at most this many goroutines, and later calls fail at
// their deadline instead of piling up behind it.
var inFlight = make(chan struct{}, 4)

// busy holds, for each item with a call running, a channel closed when it returns. A call
// starts only once its item is free, so a write abandoned at its deadline finishes before
// a later delete of that item runs: cleanup can never come first and be undone by the
// write landing late. A caller waits for the item without a slot or a goroutine.
var busy = struct {
	sync.Mutex
	items map[string]chan struct{}
}{items: map[string]chan struct{}{}}

// bounded runs op on the item name of the config directory dir with a deadline, since a
// locked Secret Service may wait on an unlock prompt indefinitely. go-keyring takes no
// context, so a late op is abandoned, not stopped: TimedOut says it may still run.
func bounded(timeout time.Duration, dir, name string, op func(s store, service, user string) (string, error)) (string, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	gaveUp := &UnavailableError{Err: fmt.Errorf("no answer in %s", timeout)}
	service, user := Service(dir), encode(name)
	key := service + "\x00" + user
	mine := make(chan struct{})
	for {
		busy.Lock()
		running, taken := busy.items[key]
		if !taken {
			busy.items[key] = mine
		}
		busy.Unlock()
		if !taken {
			break
		}
		select {
		case <-running:
		case <-deadline.C:
			return "", gaveUp
		}
	}
	free := func() {
		busy.Lock()
		delete(busy.items, key)
		busy.Unlock()
		close(mine)
	}
	select {
	case inFlight <- struct{}{}:
	case <-deadline.C:
		free()
		return "", gaveUp
	}
	type result struct {
		v   string
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			free()
			<-inFlight
		}()
		v, err := op(backend(dir), service, user)
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		switch {
		case r.err == nil:
			return r.v, nil
		case errors.Is(r.err, keyring.ErrNotFound):
			return "", ErrNotFound
		case IsUnavailable(r.err):
			return "", r.err
		default:
			return "", &UnavailableError{Err: r.err}
		}
	case <-deadline.C:
		return "", &UnavailableError{Err: fmt.Errorf("no answer in %s", timeout), TimedOut: true}
	}
}
