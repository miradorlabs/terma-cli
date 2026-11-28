package hookrun

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Every record a hook writes is in one lane, its collector's (spool.Lane), or none: then it
// is lost before anything of it is written, and counted. A git hook in an agent's sandbox,
// which can neither read the sign-in nor write the state directory, parks what it writes in
// the repository's git directory, in the lane of the sessions it is of (recordLane).

// store is the session store in dir, its manifests named for this hook's lane.
func (e Env) store(dir string) *session.Store {
	s := session.Open(dir)
	s.Bind(laneName(e.lane()))
	return s
}

// laneName is l as a manifest names it (session.Manifest.Account): an account key,
// sendas.NoPersonLane, or "" for none.
func laneName(l spool.Lane) string {
	if l.NoPerson {
		return sendas.NoPersonLane
	}
	return l.Account
}

// parking is where a git hook in an agent's sandbox parks what the state directory refused:
// the repository's git directory, which the sandbox lets it write, even where the session
// store is a workspace's under the state directory.
func (r *Repo) parking() *session.Store {
	return session.Open(filepath.Join(r.GitDir, project.GitStoreDir))
}

// What a git hook run in an agent's sandbox could not write to the state directory waits in
// the repository's git directory, which it can write, under these names: events, push
// records and the counts of what it lost. They are new to this version, so an older terma's
// hook never unparks, and strips, what this one tagged.
const (
	parkedEvents = "events-v2"
	parkedPushes = "pushes-v2"
	parkedLosses = "losses-v2"
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

// LostNoDestination counts parked or pre-cutover records dropped because they carry no
// routed enrollment destination (Council 12e).
const LostNoDestination = "no_destination"

// unpark hands on what sandboxed hooks parked in r's git directory: pushes to AwaitPush,
// the counts of what they lost to the spool's, and events to the spool, in one append, then
// a flush. When Routing is active, any parked event carrying no Enrollment is dropped and
// counted as no_destination instead of being filled in with a default team.
func (e Env) unpark(r *Repo) {
	if r.GitDir == "" {
		return
	}
	e.unparkPushes(r)
	if e.Spool == nil {
		return
	}
	parking := r.parking()
	if err := parking.Take(parkedLosses, e.Spool.ImportLosses); err != nil {
		e.Logf("unpark losses: %v", err)
	}
	moved := false
	err := parking.Unpark(parkedEvents, func(lines []byte) error {
		var events []spool.Event
		noDest := 0
		for line := range bytes.Lines(lines) {
			var ev spool.Event
			if json.Unmarshal(line, &ev) != nil {
				continue
			}
			if e.Routing != nil && ev.Enrollment == "" {
				noDest++
			} else {
				events = append(events, ev)
			}
		}
		if moved = len(events) > 0; moved {
			if err := e.Spool.AppendAsCollected(events...); err != nil {
				return err
			}
		}
		return e.countUnparkNoDest(r, moved, noDest)
	})
	if err != nil {
		e.Logf("unpark events: %v", err)
		return
	}
	if moved && e.Flush != nil {
		e.Flush()
	}
}

func (e Env) countUnparkNoDest(r *Repo, kept bool, n int) error {
	if n == 0 {
		return nil
	}
	if kept {
		e.CountLost(r, LostNoDestination, n)
		return nil
	}
	if e.Spool == nil {
		return errors.New("no spool for no_destination loss count")
	}
	return e.Spool.CountLost(LostNoDestination, n)
}

// emitRecord is EmitFor for a git hook's record of sessions, a commit's, whose manifests
// name accounts, one per session (session.Store.Accounts): it goes in the lane recordLane
// gives it, or with none is lost, counted as ambiguous_push.
func (e Env) emitRecord(r *Repo, ev spool.Event, accounts []string) bool {
	if e.lostOutside(r, 1) {
		return true
	}
	lane, ok := recordLane(e.lane(), accounts)
	if !ok {
		e.CountLost(r, sendas.LostAmbiguousPush, 1)
		return true
	}
	ev = e.stampFor(r, ev)
	ev.Account, ev.NoPerson = lane.Account, lane.NoPerson
	if e.Routing == nil {
		e.claimForRelay(r, ev, true)
	}
	return e.emitOrPark(r, ev)
}

// recordLane is the lane of a git hook's record of sessions, a commit or a push, whose
// manifests name accounts, one per session ("" for none). A record belongs to its
// collector, and is kept only where its sessions agree:
//   - a hook that read its sign-in (own) collected it, so it is in own's lane, unless a
//     session's manifest names another account;
//   - a hook that couldn't, a git hook in an agent's sandbox, has only its sessions to go
//     by: the record is in the one lane all their manifests name, and in none when a
//     session's manifest names no one, or there is no session.
//
// A session whose manifest can't be read (sendas.AccountUnreadable) may be anyone's, so a
// record of it is in no lane. ok is false for none: a record of several people's sessions,
// or of no one's, is lost.
func recordLane(own spool.Lane, accounts []string) (lane spool.Lane, ok bool) {
	if slices.Contains(accounts, sendas.AccountUnreadable) {
		return spool.Lane{}, false
	}
	if own.Lost == "" {
		for _, a := range accounts {
			if a != "" && a != laneName(own) {
				return spool.Lane{}, false
			}
		}
		return own, true
	}
	if len(accounts) == 0 {
		return spool.Lane{}, false
	}
	for _, a := range accounts {
		if a == "" || a != accounts[0] {
			return spool.Lane{}, false
		}
	}
	if accounts[0] == sendas.NoPersonLane {
		return spool.Lane{NoPerson: true}, true
	}
	return spool.Lane{Account: accounts[0]}, true
}

// claimAccount is the lane a claim this hook writes is made under (claim.Claim.Account).
func (e Env) claimAccount() string {
	if l := e.lane(); !l.NoPerson {
		return l.Account
	}
	return sendas.NoPersonLane
}

// lane is whose what this hook collects is: its spool's binding, which the hook's caller
// made from the profile (dispatch), or with no spool, the profile's own.
func (e Env) lane() spool.Lane {
	if e.Spool != nil {
		return e.Spool.Lane()
	}
	return spool.LaneOf(e.Account, e.ServerKey)
}

// lost reports whether what this hook collects has no lane, before anything of it is
// written, and if so counts its n events lost, by why.
func (e Env) lost(r *Repo, n int) bool {
	why := e.lane().Lost
	if why == "" {
		return false
	}
	e.CountLost(r, why, n)
	return true
}

// lostOutside is lost for a git hook's record of sessions, except in an agent's sandbox, where
// a hook can't write the state directory and so can't read the sign-in either: there a
// sign-in it can't read is no reason to lose the record, which takes its sessions' lane
// instead (recordLane). The sandbox is told by what it refuses, a write to the spool
// (spool.Spool.Appendable), never by whether the count went through: a hook whose spool takes
// a write read its sign-in where it could, so the record is lost, however its count fares.
// Signed out is signed out anywhere.
func (e Env) inSandbox() bool {
	return e.Spool == nil || !e.Spool.Appendable()
}

func (e Env) lostOutside(r *Repo, n int) bool {
	if e.lane().Lost == sendas.LostUnreadable && e.inSandbox() {
		return false
	}
	return e.lost(r, n)
}

// CountLost counts n events lost for why in the spool or, by a hook in an agent's sandbox
// that can't write the state directory, in r's git directory, which the next hook outside
// moves into the spool's (unpark).
func (e Env) CountLost(r *Repo, why string, n int) {
	if e.Spool != nil && e.Spool.CountLost(why, n) == nil {
		return
	}
	if r != nil && r.GitDir != "" && r.parking().Park(parkedLosses, spool.EncodeLoss(why, n, e.Time())) == nil {
		return
	}
	e.Logf("%d event(s) lost (%s), uncounted", n, why)
}
