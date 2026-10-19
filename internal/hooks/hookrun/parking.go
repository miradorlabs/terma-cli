package hookrun

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// parking is where a git hook in an agent's sandbox parks what the state directory refused:
// the repository's git directory, which the sandbox lets it write, even where the session
// store is a workspace's under the state directory.
func (r *Repo) parking() *session.Store {
	return session.Open(filepath.Join(r.GitDir, project.GitStoreDir))
}

// What a git hook run in an agent's sandbox could not write to the state directory waits in
// the repository's git directory, which it can write, under these names.
const (
	parkedEvents = "events"
	parkedPushes = "pushes"
)

// park keeps v, which the state directory refused, in the repository's git directory under name;
// the next hook there that reaches the state directory unparks it.
func (e Env) park(r *Repo, name string, v any) bool {
	if r == nil || r.GitDir == "" {
		return false // a folder outside git keeps its store in the state directory too
	}
	line, err := json.Marshal(v)
	if err == nil {
		err = r.parking().Park(name, append(line, '\n'))
	}
	if err != nil {
		e.Logf("park %s: %v", name, err)
		return false
	}
	return true
}

// unpark hands on what sandboxed hooks parked in r's git directory: events to the spool, in
// one append, then a flush, and pushes to AwaitPush. Each event is stamped for r (restamp).
func (e Env) unpark(r *Repo) {
	if r.GitDir == "" {
		return
	}
	e.unparkPushes(r)
	if e.Spool == nil {
		return
	}
	moved := false
	err := r.parking().Unpark(parkedEvents, func(lines []byte) error {
		var events []spool.Event
		for line := range bytes.Lines(lines) {
			var ev spool.Event
			if json.Unmarshal(line, &ev) == nil {
				events = append(events, e.restamp(r, ev))
			}
		}
		moved = len(events) > 0
		return e.Spool.Append(events...)
	})
	if err != nil {
		e.Logf("unpark events: %v", err)
		return
	}
	if moved && e.Flush != nil {
		e.Flush()
	}
}

// restamp stamps an event parked in r's git directory as a hook in r stamps one. The git
// directory is an agent's sandbox's to write, so the project, repository and coverage the
// line names are not taken, except a Global already set: it only withholds the event once
// the policy leaves global mode, as it would have before it was parked.
func (e Env) restamp(r *Repo, ev spool.Event) spool.Event {
	global := ev.Global
	delete(ev.Attrs, AttrProjectID)
	ev = e.stamp(r, ev)
	ev.Global = ev.Global || global
	return ev
}

// maxUnparkedPushes bounds the pushes one hook records from the parking, and so the
// AwaitPush it starts: pre-push parks one per push, and hooks outside the sandbox unpark as
// they run.
const maxUnparkedPushes = 64

// unparkPushes records the pushes a sandboxed pre-push parked in r's git directory and starts their
// AwaitPush, which reports each at once, its git push long gone. A record pre-push could not
// have parked there is dropped (parkedPush), as is any past maxUnparkedPushes; one already
// recorded is awaited as it is.
func (e Env) unparkPushes(r *Repo) {
	if e.AwaitPush == nil {
		return
	}
	var recorded []string
	err := r.parking().Unpark(parkedPushes, func(lines []byte) error {
		for line := range bytes.Lines(lines) {
			var rec pushRecord
			if json.Unmarshal(line, &rec) != nil {
				continue
			}
			rec, ok := e.parkedPush(r, rec)
			if !ok || len(recorded) == maxUnparkedPushes {
				e.Logf("unpark pushes: dropped a record pre-push did not write, or one too many")
				continue
			}
			path := filepath.Join(e.StateDir, PushesDir, rec.ID+".json")
			if _, err := os.Lstat(path); err == nil {
				recorded = append(recorded, path)
				continue
			}
			if err := config.WriteJSON(path, rec, 0o600); err != nil {
				return err
			}
			recorded = append(recorded, path)
		}
		return nil
	})
	if err != nil {
		// Still parked: the next hook records them all again and awaits each once.
		e.Logf("unpark pushes: %v", err)
		return
	}
	for _, path := range recorded {
		e.AwaitPush(path)
	}
}

// parkedPush holds a record parked in r's git directory, which an agent's sandbox may write,
// to what PrePush parks there: an id it mints, so the record lands in PushesDir; the branch
// updates ParsePushInput keeps, with full commit ids, and remote-tracking refs inside
// refs/remotes. A time to come is now, which bounds the wait and the retries. Its repository
// is r, whatever it names, and its event is stamped as a hook in r stamps one (restamp).
// ok is false for a record that is not such a push.
func (e Env) parkedPush(r *Repo, rec pushRecord) (pushRecord, bool) {
	if !validPushID(rec.ID) || len(rec.Refs) == 0 || rec.Event.Name != semconv.TermaPushEvent {
		return rec, false
	}
	for _, ref := range rec.Refs {
		if !ref.reportable() || !validTracking(ref) {
			return rec, false
		}
	}
	for _, sha := range rec.Excluded {
		if !gitx.ValidOID(sha) {
			return rec, false
		}
	}
	// The clock, not the hook's start: pre-push may have parked it since this hook started.
	if now := time.Now(); rec.Time.After(now) {
		rec.Time = now
	}
	rec.Root, rec.GitDir = r.Root, r.GitDir
	rec.Event = e.restamp(r, rec.Event)
	return rec, true
}

// pushIDAlphabet is rand.Text's, RFC 4648 base32: an id in it is a file name and no path.
const pushIDAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"

// pushIDMin is rand.Text's length today, 128 bits in base32, and pushIDMax leaves room for
// the longer text its documentation allows a later Go to return.
const pushIDMin, pushIDMax = 26, 64

// validPushID reports whether id is one rand.Text mints.
func validPushID(id string) bool {
	return len(id) >= pushIDMin && len(id) <= pushIDMax && strings.Trim(id, pushIDAlphabet) == ""
}

// validTracking reports whether ref's remote-tracking ref is one PrePush reads: none, or a
// ref under refs/remotes/ that stays there, with its commit id when it existed.
func validTracking(ref PushRef) bool {
	if ref.TrackingBefore != "" && !gitx.ValidOID(ref.TrackingBefore) {
		return false
	}
	if ref.Tracking == "" {
		return true
	}
	name, ok := strings.CutPrefix(ref.Tracking, "refs/remotes/")
	if !ok || strings.ContainsAny(name, "\\\x00") {
		return false
	}
	for part := range strings.SplitSeq(name, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}
