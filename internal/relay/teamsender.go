package relay

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

func (r *Relay) teamSender(rt teamRoute) *sender {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.teamSenders[rt]; s != nil {
		return s
	}
	s := &sender{r: r, team: &rt, dir: r.outbox.teamDir(rt), name: rt.String(), queue: map[Signal][]entry{}, wake: make(chan struct{}, 1), stateChange: make(chan struct{}, 1)}
	r.teamSenders[rt] = s
	r.wg.Add(1)
	go s.loop()
	return s
}
func (r *Relay) enqueueTeam(q Queued, tool string, p *part) {
	q.Account = cmp.Or(q.Account, p.account)
	rt := teamRoute{q.Account, q.Enrollment, cmp.Or(tool, noTool)}
	body, err := proto.Marshal(p.msg)
	if err != nil {
		r.stats.dropped(p.signal, "encode_failed", p.records)
		return
	}
	e := newEntry(nonZeroTime(p.arrived, r.opts.Now()), p.signal, p.records)
	e.q = q
	if err := r.outbox.putTeam(rt, q, e, body); err != nil {
		r.stats.dropped(p.signal, "outbox_write_failed", p.records)
		r.warnf("outbox %s: %v; dropped %d %s", rt, err, p.records, p.signal)
		return
	}
	s := r.teamSender(rt)
	s.push(e)
	s.mu.Lock()
	blocked := s.blocked
	s.keyless = s.keyless && blocked
	s.mu.Unlock()
	if !blocked {
		s.poke()
	}
}
func (s *sender) readFile(e entry) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, e.name))
	if err != nil || s.team == nil {
		return data, err
	}
	if _, body, ok := bytes.Cut(data, []byte("\n")); ok {
		return body, nil
	}
	return nil, errors.New("a queued part without its destination")
}
func (s *sender) removeFiles(batch []entry) {
	for _, e := range batch {
		_ = config.Remove(filepath.Join(s.dir, e.name))
	}
	s.forget(batch)
}
func (s *sender) buryFile(e entry) {
	dead, src := filepath.Join(s.r.outbox.dir, deadDir), filepath.Join(s.dir, e.name)
	if os.MkdirAll(dead, 0o700) != nil || config.Rename(src, filepath.Join(dead, strings.ReplaceAll(s.name, "/", "-")+"-"+e.name)) != nil {
		_ = config.Remove(src)
	}
	s.forget([]entry{e})
}
func (s *sender) dropMatching(match func(entry) bool, why, blocker string) {
	s.mu.Lock()
	var es []entry
	for sig, q := range s.queue {
		s.queue[sig] = slices.DeleteFunc(q, func(e entry) bool {
			if match(e) {
				es = append(es, e)
				s.r.dropStats(e.signal, why, blocker, e.records)
				return true
			}
			return false
		})
	}
	s.mu.Unlock()
	for _, e := range es {
		_ = config.Remove(filepath.Join(s.dir, e.name))
	}
}
func (s *sender) checkEntry(rc Rechecker, e entry) Verdict {
	v := rc.Entry(e.q)
	if v.Verdict == Discard {
		s.r.dropStats(e.signal, v.Why.reason(), v.Blocker, e.records)
		s.removeFiles([]entry{e})
	}
	return v.Verdict
}
func (s *sender) checkTeamWaits(rc Rechecker) (discarded, waited bool) {
	tw, waits := rc.TeamWaits()
	if waits && tw.Verdict == Discard {
		s.dropMatching(func(entry) bool { return true }, tw.Why.reason(), tw.Blocker)
		return true, false
	}
	return false, waits
}
func entryCmp(e entry, name string) int { return strings.Compare(e.name, name) }
func (s *sender) nextAfter(after map[Signal]string, only Signal) (best entry, found bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sig, q := range s.queue {
		if only != "" && sig != only || len(q) == 0 {
			continue
		}
		i := 0
		if cur := after[sig]; cur != "" {
			idx, eq := slices.BinarySearchFunc(q, cur, entryCmp)
			i = idx + map[bool]int{true: 1}[eq]
		}
		if i < len(q) && (!found || q[i].name < best.name) {
			best, found = q[i], true
		}
	}
	return best, found
}
func (s *sender) resolveUnread(e *entry) (ready, wait bool) {
	switch q, err := readTeamHeader(filepath.Join(s.dir, e.name), s.team.account); {
	case errors.Is(err, os.ErrNotExist):
		s.forget([]entry{*e})
	case errors.Is(err, errCorruptHeader):
		s.r.stats.dropped(e.signal, "outbox_unreadable", e.records)
		s.buryFile(*e)
	case err != nil:
		return false, true
	default:
		e.q, e.unread = q, false
		s.mu.Lock()
		if i, hit := slices.BinarySearchFunc(s.queue[e.signal], e.name, entryCmp); hit {
			s.queue[e.signal][i] = *e
		}
		s.mu.Unlock()
		return true, false
	}
	return false, false
}
func (s *sender) nextBatch(limit int) ([]entry, time.Duration) {
	if s.team == nil {
		return s.next(limit), idleWait
	}
	if _, has := s.nextAfter(nil, ""); !has {
		s.scanAfter, s.scanFound, s.scanWait = nil, false, false
		return nil, idleWait
	}
	if !s.r.teams() || !s.r.laneActive(s.team.account) {
		s.setKeyless(true)
		return nil, keylessRetry
	}
	rc := s.r.opts.Router.Recheck(s.team.enrollment, s.team.account)
	s.gen = rc.Generation()
	if discarded, waited := s.checkTeamWaits(rc); discarded || waited {
		s.setKeyless(waited)
		return nil, map[bool]time.Duration{true: keylessRetry, false: idleWait}[waited]
	}
	after := map[Signal]string{}
	maps.Copy(after, s.scanAfter)
	entries, firstSig, firstProj, waiting := []entry(nil), Signal(""), "", false
	for scanned := 0; len(entries) < limit && scanned < max(limit*4, maxMergeFiles); scanned++ {
		e, ok := s.nextAfter(after, firstSig)
		if !ok {
			if len(entries) > 0 {
				break
			}
			hadWork, wasWait := s.scanFound, waiting || s.scanWait
			s.scanAfter, s.scanFound, s.scanWait = nil, false, false
			if hadWork {
				return nil, 0
			}
			s.mu.Lock()
			s.keyless, s.blocked = wasWait, false
			s.mu.Unlock()
			return nil, map[bool]time.Duration{true: keylessRetry, false: idleWait}[wasWait]
		}
		after[e.signal] = e.name
		ready, w := !e.unread, false
		if e.unread {
			ready, w = s.resolveUnread(&e)
			waiting = waiting || w
		}
		if ready && (len(entries) == 0 || e.q.Project == firstProj) {
			if v := s.checkEntry(rc, e); v == SendNow {
				firstSig, firstProj, entries, s.scanFound = cmp.Or(firstSig, e.signal), cmp.Or(firstProj, e.q.Project), append(entries, e), true
				continue
			} else if v == Wait {
				waiting = true
			}
		}
		if len(entries) == 0 {
			s.scanAfter = maps.Clone(after)
		}
	}
	if len(entries) == 0 {
		s.scanAfter, s.scanWait = after, s.scanWait || waiting
		return nil, 0
	}
	s.setKeyless(false)
	return entries, idleWait
}
func (s *sender) resolveBatchPolicy(c claim.Claim) (Policy, bool) {
	if s.team == nil {
		pol, ok := s.r.resolve(c, s.route.account)
		return pol, ok && s.r.mayLeave(s.route, pol.Key != "")
	}
	return s.r.teamReady(s.team.enrollment, s.team.toolLabel(), s.team.account, true)
}
func (s *sender) postBatch(ctx context.Context, pol Policy, batch []entry, body []byte) (outcome, time.Duration, string, int64) {
	if s.team == nil {
		return s.send(ctx, pol, batch[0], body, nil)
	}
	return s.send(ctx, pol, batch[0], body, func() (outcome, time.Duration, string) {
		if !s.r.laneActive(s.team.account) || !s.r.mayLeaveAccount(s.team.account, pol.Key != "") {
			return keyless, keylessRetry, "lane changed before send"
		}
		rc := s.r.opts.Router.Recheck(s.team.enrollment, s.team.account)
		if discarded, waited := s.checkTeamWaits(rc); discarded || waited {
			return map[bool]outcome{true: aborted, false: keyless}[discarded], map[bool]time.Duration{true: 0, false: keylessRetry}[discarded], map[bool]string{true: "", false: "team waiting before send"}[discarded]
		}
		var discarded, waited bool
		for _, e := range batch {
			v := s.checkEntry(rc, e)
			discarded, waited = discarded || v == Discard, waited || v == Wait
		}
		if genChanged := rc.Generation() != s.gen; discarded || genChanged {
			if genChanged {
				s.r.invalidateTeamPolicy(s.team.enrollment)
			}
			return aborted, 0, ""
		}
		if waited {
			return keyless, keylessRetry, "entry waiting before send"
		}
		return sent, 0, ""
	})
}
func (r *Relay) dropMixed(session, run string, from time.Time) {
	key := session + "\x00" + run
	r.mu.Lock()
	if seen, ok := r.mixedSeen[key]; ok && !from.Before(seen) {
		r.mu.Unlock()
		return
	}
	if len(r.mixedSeen) >= maxTraces {
		clear(r.mixedSeen)
	}
	r.mixedSeen[key] = from
	r.mu.Unlock()
	for _, s := range r.allTeamSenders() {
		s.dropMatching(func(e entry) bool {
			return e.q.Session == session && (run == "" || e.q.Run == "" || e.q.Run == run) && (e.q.EffectiveEnd().IsZero() || !e.q.EffectiveEnd().Before(from))
		}, string(WhyMixed), "mixed:"+session)
	}
}
func (r *Relay) dropDeselected() {
	if active := r.account(); r.teams() && active != "" && active != sendas.AccountUnreadable {
		for _, s := range r.allTeamSenders() {
			if s.team.account == active && !r.opts.Router.Selected(s.team.enrollment, s.team.account) {
				s.dropMatching(func(entry) bool { return true }, string(WhyDeselected), "")
			}
		}
	}
}
