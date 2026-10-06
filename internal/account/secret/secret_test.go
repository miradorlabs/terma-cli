package secret

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSetGetDelete(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := Get(dir, "key/p1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get before Set = %v, want ErrNotFound", err)
	}
	if err := Set(dir, "key/p1", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := Set(dir, "key/p1", "v2"); err != nil {
		t.Fatal(err)
	}
	if v, err := Get(dir, "key/p1"); err != nil || v != "v2" {
		t.Fatalf("Get = %q, %v; want the latest value", v, err)
	}
	if v, err := Get(t.TempDir(), "key/p1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another config directory read %q, %v", v, err)
	}
	if err := Delete(dir, "key/p1"); err != nil {
		t.Fatal(err)
	}
	if err := Delete(dir, "key/p1"); err != nil {
		t.Fatalf("deleting a missing item: %v", err)
	}
	if _, err := Get(dir, "key/p1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete = %v", err)
	}
}

// A store that cannot be reached is never mistaken for a missing secret.
func TestAnUnreachableStoreIsNotAMissingSecret(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	FailForTest(t, dir)
	_, err := Get(dir, "key/p1")
	if !IsUnavailable(err) || errors.Is(err, ErrNotFound) {
		t.Fatalf("Get = %v, want unavailable", err)
	}
	if err := Set(dir, "key/p1", "v"); !IsUnavailable(err) {
		t.Fatalf("Set = %v, want unavailable", err)
	}
	if err := Delete(dir, "key/p1"); !IsUnavailable(err) {
		t.Fatalf("Delete = %v, want unavailable", err)
	}
}

// Names reach a backend with nothing it could misread, such as a newline that would end
// macOS's `security -i` line early, and two names never share an item.
func TestNamesAreEncodedForEveryBackend(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"credential/default/org-1":         "credential/default/org-1",
		"/Users/a b/.config/terma":         "/Users/a%20b/.config/terma",
		`C:\Users\a\AppData\Roaming\terma`: "C:%5CUsers%5Ca%5CAppData%5CRoaming%5Cterma",
		"credential/o'rg \"x\"\nadd":       "credential/o%27rg%20%22x%22%0Aadd",
		"100%":                             "100%25",
	} {
		if got := encode(in); got != want {
			t.Errorf("encode(%q) = %q, want %q", in, got, want)
		}
	}
	dir := t.TempDir()
	if err := Set(dir, "key/a b", "1"); err != nil {
		t.Fatal(err)
	}
	if v, err := Get(dir, "key/a%20b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a percent-encoded look-alike read %q, %v", v, err)
	}
}

// A store that never answers times out, and holds no more than inFlight calls however
// often it is asked: later calls fail at their deadline without starting another. Not
// parallel, since it fills the package's slots until it ends.
func TestAWedgedStoreHoldsBoundedCalls(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	var started, finished sync.WaitGroup
	var calls atomic.Int32
	wedged := func(store, string, string) (string, error) {
		calls.Add(1)
		started.Done()
		<-release
		finished.Done()
		return "", nil
	}
	started.Add(cap(inFlight))
	finished.Add(cap(inFlight))
	for i := range 3 * cap(inFlight) {
		if _, err := bounded(5*time.Millisecond, dir, fmt.Sprintf("key/p%d", i), wedged); !IsUnavailable(err) {
			t.Fatalf("bounded = %v, want unavailable", err)
		}
	}
	started.Wait()
	if n := calls.Load(); n != int32(cap(inFlight)) {
		t.Fatalf("a wedged store was called %d times, want at most %d", n, cap(inFlight))
	}
	close(release)
	finished.Wait()
	if _, err := bounded(time.Second, dir, "key/after", func(store, string, string) (string, error) { return "", nil }); err != nil {
		t.Fatalf("once the store answers again: %v", err)
	}
}

// Calls waiting on an item a wedged call holds take no slot: however many retries queue
// behind it, another item is still answered. Not parallel, since it holds a slot.
func TestWaitersOnAWedgedItemLeaveOtherItemsAnswered(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	defer close(release)
	if _, err := bounded(5*time.Millisecond, dir, "key/wedged", func(store, string, string) (string, error) {
		<-release
		return "", nil
	}); !MayLand(err) {
		t.Fatalf("the wedged call = %v", err)
	}
	for range 2 * cap(inFlight) {
		if _, err := bounded(5*time.Millisecond, dir, "key/wedged", func(store, string, string) (string, error) {
			t.Error("a call ran while its item was still held")
			return "", nil
		}); !IsUnavailable(err) || MayLand(err) {
			t.Fatalf("a waiter = %v, want unavailable and never started", err)
		}
	}
	if err := Set(dir, "key/other", "v"); err != nil {
		t.Fatalf("another item was starved: %v", err)
	}
}

// A write abandoned at its deadline still runs before a later delete of the same item, so
// the delete cannot come first and be undone when the write lands.
func TestALateWriteRunsBeforeALaterDelete(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	release := make(chan struct{})
	_, err := bounded(5*time.Millisecond, dir, "key/p1", func(s store, service, user string) (string, error) {
		<-release
		return "", s.Set(service, user, "late")
	})
	if !MayLand(err) {
		t.Fatalf("the slow write = %v, want one that may land", err)
	}
	deleted := make(chan error, 1)
	go func() { deleted <- Delete(dir, "key/p1") }()
	close(release)
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	if v, err := Get(dir, "key/p1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the late write outlived the delete issued after it: %q, %v", v, err)
	}
}

// A value read after the deadline is never handed to a caller that gave up on it.
func TestATimedOutReadReturnsNothing(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	defer close(release)
	v, err := bounded(5*time.Millisecond, t.TempDir(), "key/p1", func(store, string, string) (string, error) {
		<-release
		return "late", nil
	})
	if v != "" || !IsUnavailable(err) {
		t.Fatalf("bounded = %q, %v", v, err)
	}
}

// TestTheSystemKeychain round-trips a secret through this machine's real store. It runs
// only with TERMA_KEYCHAIN_TEST=1: CI's Windows job, and by hand on macOS and Linux.
func TestTheSystemKeychain(t *testing.T) {
	if os.Getenv("TERMA_KEYCHAIN_TEST") != "1" {
		t.Skip("set TERMA_KEYCHAIN_TEST=1 to use this machine's keychain")
	}
	useSystemInTests = true
	t.Cleanup(func() { useSystemInTests = false })
	dir := t.TempDir()
	// Quotes, spaces and a newline name one item intact, as a profile name may carry them.
	const name, value = "credential/o'rg \"x\" y\nadd-generic-password -s injected", `{"access_token":"a'b \"c\""}`
	if err := Set(dir, name, value); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Delete(dir, name) })
	if v, err := Get(dir, name); err != nil || v != value {
		t.Fatalf("Get = %q, %v", v, err)
	}
	if err := Delete(dir, name); err != nil {
		t.Fatal(err)
	}
	if _, err := Get(dir, name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
}
