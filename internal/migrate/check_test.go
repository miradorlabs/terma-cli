package migrate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// installs is a relay's terma and the other termas a hook entry could name: a copy of the
// same build, a link to it, an older build, and a path where nothing is installed.
type installs struct {
	relay, copy, link, older, gone string
}

func newInstalls(t *testing.T) installs {
	t.Helper()
	dir := t.TempDir()
	in := installs{relay: filepath.Join(dir, "bin", "terma"), copy: filepath.Join(dir, "npm", "terma"),
		link: filepath.Join(dir, "brew", "terma"), older: filepath.Join(dir, "local", "terma"), gone: filepath.Join(dir, "removed", "terma")}
	for path, build := range map[string]string{in.relay: "build 2", in.copy: "build 2", in.older: "build 1"} {
		must(t, os.MkdirAll(filepath.Dir(path), 0o700))
		must(t, os.WriteFile(path, []byte(build), 0o700))
	}
	must(t, os.MkdirAll(filepath.Dir(in.link), 0o700))
	must(t, os.Symlink(in.relay, in.link))
	return in
}

// hooked is a Machine.Hooked that lists paths.
func hooked(paths ...string) func() ([]string, error) {
	return func() ([]string, error) { return paths, nil }
}

// With two installs the machine is not migrated: nothing changes, and doctor's record
// names the relay's terma and the one the hooks run. Once the hooks run the relay's
// terma, the next start migrates and the record goes.
func TestTwoInstallsBlockTheMigration(t *testing.T) {
	t.Parallel()
	in := newInstalls(t)
	m := severalMachine(t)
	m.Executable = in.relay
	m.Hooked = hooked(in.relay, in.older, in.older)
	before := snapshot(t, m)
	if out, err := Run(context.Background(), m); err != nil || out != NotMigrated {
		t.Fatalf("Run with two installs = %s, %v", out, err)
	}
	if after := snapshot(t, m); !mapsEqual(before, after) || config.Migrated(m.ConfigDir) {
		t.Fatal("two installs still changed the machine")
	}
	b, ok := ReadBlock(m.StateDir)
	if !ok || b.Reason != TwoInstalls || b.Executable != in.relay || !slices.Equal(b.Others, []string{in.older}) || b.At.IsZero() {
		t.Fatalf("doctor's record = %+v, %v", b, ok)
	}
	if !Needed(m.ConfigDir, m.StateDir) {
		t.Fatal("a blocked machine needs no migration")
	}

	m.Hooked = hooked(in.relay)
	m.commit(t)
	if _, ok := ReadBlock(m.StateDir); ok {
		t.Fatal("the record outlived the migration")
	}
}

// The relay's own terma, reached by a link or copied as the same build, is one install,
// and a hook naming a terma that is gone runs nothing: none of these blocks.
func TestOneInstallByAnyPathMigrates(t *testing.T) {
	t.Parallel()
	in := newInstalls(t)
	for name, path := range map[string]string{"link": in.link, "copy": in.copy, "gone": in.gone, "itself": in.relay} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := severalMachine(t)
			m.Executable, m.Hooked = in.relay, hooked(path)
			m.commit(t)
		})
	}
}

// Hook entries that cannot be read leave the machine as it was: which terma they run is
// not known.
func TestUnreadableHooksBlockTheMigration(t *testing.T) {
	t.Parallel()
	m := severalMachine(t)
	m.Hooked = func() ([]string, error) { return nil, errors.New("permission denied") }
	if out, err := Run(context.Background(), m); err != nil || out != NotMigrated || config.Migrated(m.ConfigDir) {
		t.Fatalf("Run = %s, %v", out, err)
	}
	if b, ok := ReadBlock(m.StateDir); !ok || b.Reason != Unchecked {
		t.Fatalf("doctor's record = %+v, %v", b, ok)
	}
}

// Once the legacy entries have begun to go, a second install no longer stops the
// migration: there is no going back, and finishing is what leaves the older terma nothing
// to collect for.
func TestTwoInstallsDoNotStopAMigrationUnderWay(t *testing.T) {
	t.Parallel()
	in := newInstalls(t)
	m := severalMachine(t)
	m.fault = crashAt("retire-keys")
	if _, err := Run(context.Background(), m); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	m.fault, m.Executable, m.Hooked = nil, in.relay, hooked(in.older)
	m.commit(t)
}

// holdFlush runs a spool flush on m's spool whose send waits until release is closed,
// and returns once it holds the flush's lock. The flush's result arrives on done.
func holdFlush(t *testing.T, m Machine) (release chan struct{}, done chan spool.Result) {
	t.Helper()
	sp, err := spool.Open(filepath.Join(m.StateDir, spool.Dir))
	must(t, err)
	must(t, sp.Append(spool.Event{Time: time.Now(), Name: "terma.session.start", Attrs: map[string]any{"project_id": acme.team}}))
	sending, release, done := make(chan struct{}), make(chan struct{}), make(chan spool.Result, 1)
	go func() {
		done <- sp.Flush(context.Background(), spool.SenderFunc(func(context.Context, []spool.Event) ([]spool.Event, error) {
			close(sending)
			<-release
			return nil, nil
		}), spool.FlushOptions{Timeout: time.Minute, Force: true})
	}()
	<-sending
	return release, done
}

// A flush already running is waited out: the migration starts once it ends, so nothing
// is sent while it runs.
func TestARunningFlushIsWaitedOut(t *testing.T) {
	t.Parallel()
	m := severalMachine(t)
	m.FlushWait = time.Minute
	release, done := holdFlush(t, m)
	migrated := make(chan Outcome, 1)
	go func() {
		out, _ := Run(context.Background(), m)
		migrated <- out
	}()
	select {
	case out := <-migrated:
		t.Fatalf("the migration ran beside a flush: %s", out)
	case <-time.After(200 * time.Millisecond):
	}
	if config.Migrated(m.ConfigDir) {
		t.Fatal("the migration began beside a flush")
	}
	close(release)
	if r := <-done; r.Err != nil || r.Sent != 1 {
		t.Fatalf("the flush = %+v", r)
	}
	if out := <-migrated; out != Committed {
		t.Fatalf("after the flush the migration = %s", out)
	}
	for _, l := range several {
		m.migrated(t, l)
	}
}

// A flush that keeps its lock past the wait leaves the machine as it was, and the next
// start migrates.
func TestAFlushThatOutlastsTheWaitDefersTheMigration(t *testing.T) {
	t.Parallel()
	m := severalMachine(t)
	m.FlushWait = 100 * time.Millisecond
	release, done := holdFlush(t, m)
	before := snapshot(t, m)
	if out, err := Run(context.Background(), m); err != nil || out != NotMigrated {
		t.Fatalf("Run beside a flush = %s, %v", out, err)
	}
	if after := snapshot(t, m); !mapsEqual(before, after) {
		t.Fatal("the migration changed the machine beside a flush")
	}
	if b, ok := ReadBlock(m.StateDir); !ok || b.Reason != FlushBusy {
		t.Fatalf("doctor's record = %+v, %v", b, ok)
	}
	close(release)
	<-done
	m.commit(t)
}

// A relay stopped while it waits for a flush stops waiting, and changes nothing.
func TestAStoppedRelayStopsWaiting(t *testing.T) {
	t.Parallel()
	m := severalMachine(t)
	m.FlushWait = time.Minute
	release, done := holdFlush(t, m)
	defer func() { close(release); <-done }()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if out, err := Run(ctx, m); !errors.Is(err, context.DeadlineExceeded) || out != NotMigrated || config.Migrated(m.ConfigDir) {
		t.Fatalf("Run = %s, %v", out, err)
	}
}
