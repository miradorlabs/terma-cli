package secret

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zalando/go-keyring"
)

// backend is the system store, except under go test, where every package's tests share an
// in-memory one so that none can reach a developer's real keychain.
func backend(dir string) store {
	if !testing.Testing() || useSystemInTests {
		return systemStore{}
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	switch memory.failing[dir] {
	case locked:
		return failingStore{err: errLocked}
	case slow:
		return failingStore{err: &UnavailableError{Err: errors.New("no answer"), TimedOut: true}}
	}
	return &memory
}

// useSystemInTests lets this package's opt-in test reach the real store.
var useSystemInTests bool

var memory = memoryStore{items: map[string]string{}, failing: map[string]failure{}}

// errRefused is a keychain declining one item while taking others, as one that will not store
// a secret that large would.
var errRefused = errors.New("the keychain refused the item")

type failure int

const (
	locked failure = iota + 1
	slow
)

// memoryStore is go-keyring's mock made safe for parallel tests.
type memoryStore struct {
	mu      sync.Mutex
	items   map[string]string
	failing map[string]failure
	// refusing are service and item-name prefixes whose items Set refuses (RefuseForTest).
	refusing []string
}

func (m *memoryStore) Set(service, user, password string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if slices.ContainsFunc(m.refusing, func(prefix string) bool { return strings.HasPrefix(service+"\x00"+user, prefix) }) {
		return errRefused
	}
	m.items[service+"\x00"+user] = password
	return nil
}

func (m *memoryStore) Get(service, user string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.items[service+"\x00"+user]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return v, nil
}

func (m *memoryStore) Delete(service, user string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[service+"\x00"+user]; !ok {
		return keyring.ErrNotFound
	}
	delete(m.items, service+"\x00"+user)
	return nil
}

type failingStore struct{ err error }

var errLocked = errors.New("the keychain is locked")

func (f failingStore) Set(string, string, string) error   { return f.err }
func (f failingStore) Get(string, string) (string, error) { return "", f.err }
func (f failingStore) Delete(string, string) error        { return f.err }

// FailForTest makes the store unreachable for the config directory dir until t ends or
// unlock is called, as a locked keychain or a session without one would be. Only for tests.
func FailForTest(t testing.TB, dir string) (unlock func()) { return failFor(t, dir, locked) }

// TimeOutForTest makes every call for dir time out, as one waiting on an unlock prompt
// would, until t ends or unlock is called. Only for tests.
func TimeOutForTest(t testing.TB, dir string) (unlock func()) { return failFor(t, dir, slow) }

func failFor(t testing.TB, dir string, f failure) (unlock func()) {
	memory.mu.Lock()
	memory.failing[dir] = f
	memory.mu.Unlock()
	unlock = func() {
		memory.mu.Lock()
		delete(memory.failing, dir)
		memory.mu.Unlock()
	}
	t.Cleanup(unlock)
	return unlock
}

// RefuseForTest makes the store refuse to keep any item for the config directory dir whose
// name starts with prefix, while it keeps every other, until t ends. Only for tests.
func RefuseForTest(t testing.TB, dir, prefix string) {
	refused := Service(dir) + "\x00" + prefix
	memory.mu.Lock()
	memory.refusing = append(memory.refusing, refused)
	memory.mu.Unlock()
	t.Cleanup(func() {
		memory.mu.Lock()
		memory.refusing = slices.DeleteFunc(memory.refusing, func(p string) bool { return p == refused })
		memory.mu.Unlock()
	})
}
