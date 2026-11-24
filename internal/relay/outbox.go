package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// The outbox is plain files, one part each, renamed into place so a crash leaves a whole file or none:
//
//	<dir>/@<account>/<project>/<tool>/<repo>/<received>-<seq>-<signal>-<records>.pb   accepted, not yet delivered
//	<dir>/@<account>/<project>/<tool>/<repo>/identity.json                            the repository <repo> hashes
//	<dir>/.dead/<account>-<project>-<tool>-<repo>-<name>                              refused by the host
//
// <account> is the parts' lane: the account key (auth.AccountKey) of the sign-in that
// collected them, so they leave only under it; or no_person (sendas.NoPersonLane), for what
// was collected under a server key, which only a server key sends. This version never
// writes a route without a lane, <dir>/<project>/...: one found there is never sent, and
// ages out (mayLeave).
//
// <received> is zero-padded Unix nanoseconds, so a name sort is arrival order. Only claimed
// parts, already filtered, are written; unsynced, so a power cut can lose the last seconds.
// <repo> keeps the claim's repository so the sender rechecks it against the policy then.

const (
	deadDir  = ".dead"
	noTool   = "_"
	noRepo   = "_"
	pbSuffix = ".pb"
	identity = "identity.json"
	repoKeyN = 16
)

// A machine offline for weeks, or a key refused for good, would otherwise fill the disk.
const (
	maxOutboxBytes = 256 << 20
	maxDeadBytes   = 32 << 20
	maxOutboxAge   = 14 * 24 * time.Hour
)

// route is where a part goes: the account that collected it, a project, the tool that
// claimed it and a hash of the repository its session ran in.
type route struct {
	account string
	project string
	tool    string
	repo    string
}

func routeOf(account string, c claim.Claim) route {
	t := c.Tool
	if t == "" {
		t = noTool
	}
	return route{account: account, project: c.ProjectID, tool: t, repo: repoKey(c.Repository)}
}

func repoKey(r config.Repository) string {
	if r == (config.Repository{}) {
		return noRepo
	}
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:repoKeyN]
}

func (rt route) toolLabel() string {
	if rt.tool == noTool {
		return ""
	}
	return rt.tool
}

func (rt route) String() string {
	s := rt.project + "/" + rt.tool + "/" + rt.repo
	if rt.account != "" {
		s = "@" + rt.account + "/" + s
	}
	return s
}

type entry struct {
	name            string
	signal          Signal
	records         int
	q               Queued
	corrupt, unread bool
}

var seq atomic.Uint64

func newEntry(now time.Time, sig Signal, records int) entry {
	n := seq.Add(1) % 1_000_000
	return entry{name: fmt.Sprintf("%020d-%06d-%s-%d%s", now.UnixNano(), n, sig, records, pbSuffix), signal: sig, records: records}
}

// received is when the entry's part arrived, from its name.
func (e entry) received() time.Time {
	ns, _, _ := strings.Cut(e.name, "-")
	n, err := strconv.ParseInt(ns, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func parseEntry(name string) (entry, bool) {
	stem, ok := strings.CutSuffix(name, pbSuffix)
	if !ok || strings.HasPrefix(name, ".") {
		return entry{}, false
	}
	f := strings.Split(stem, "-")
	if len(f) != 4 {
		return entry{}, false
	}
	if _, err := strconv.ParseInt(f[0], 10, 64); err != nil {
		return entry{}, false
	}
	sig := Signal(f[2])
	if sig != Logs && sig != Metrics && sig != Traces {
		return entry{}, false
	}
	n, err := strconv.Atoi(f[3])
	if err != nil || n < 0 {
		return entry{}, false
	}
	return entry{name: name, signal: sig, records: n}, true
}

type outbox struct{ dir string }

func (o outbox) routeDir(rt route) string {
	if rt.account != "" {
		return filepath.Join(o.dir, accountPrefix+rt.account, rt.project, rt.tool, rt.repo)
	}
	return filepath.Join(o.dir, rt.project, rt.tool, rt.repo)
}

// accountPrefix starts an account's directory, which no project id does.
const accountPrefix = "@"

// validRoute admits a route's directory names: an account key or none, a project id, a
// tool label and a repository key.
func validRoute(rt route) bool {
	return validAccount(rt.account) && project.ValidID(rt.project) && (rt.tool == noTool || project.ValidID(rt.tool)) && validRepoKey(rt.repo)
}

func validAccount(a string) bool {
	return a == "" || a == sendas.NoPersonLane || auth.ValidAccountKey(a)
}

func validRepoKey(k string) bool {
	if k == noRepo {
		return true
	}
	return len(k) == repoKeyN && strings.Trim(k, "0123456789abcdef") == ""
}

// put writes a part, and first, for a new route, the repository it was claimed in.
func (o outbox) put(rt route, repo config.Repository, e entry, body []byte) error {
	if o.dir == "" {
		return errors.New("the relay has no outbox directory")
	}
	if !validRoute(rt) {
		return fmt.Errorf("unusable route %q", rt)
	}
	dir := o.routeDir(rt)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if rt.repo != noRepo {
		if _, err := os.Stat(filepath.Join(dir, identity)); errors.Is(err, fs.ErrNotExist) {
			b, err := json.Marshal(repo)
			if err != nil {
				return err
			}
			if err := config.WriteFileAtomicNoSync(filepath.Join(dir, identity), b, 0o600); err != nil {
				return err
			}
		}
	}
	return config.WriteFileAtomicNoSync(filepath.Join(dir, e.name), body, 0o600)
}

func (o outbox) list(rt route) ([]entry, error) {
	des, err := os.ReadDir(o.routeDir(rt))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []entry
	for _, de := range des {
		if !de.Type().IsRegular() {
			continue
		}
		if e, ok := parseEntry(de.Name()); ok {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b entry) int { return strings.Compare(a.name, b.name) })
	return out, nil
}

// identity is the repository a route's parts were claimed in; the zero one for noRepo, or
// when unreadable, which a repository list never admits.
func (o outbox) identity(rt route) config.Repository {
	var repo config.Repository
	if rt.repo == noRepo {
		return repo
	}
	b, err := os.ReadFile(filepath.Join(o.routeDir(rt), identity))
	if err != nil || json.Unmarshal(b, &repo) != nil {
		return config.Repository{}
	}
	return repo
}

// remove deletes delivered parts. A part left behind is sent again, so on Windows the delete
// waits out a handle someone else holds, such as a virus scanner reading the new file.
func (o outbox) remove(rt route, batch []entry) {
	for _, e := range batch {
		_ = config.Remove(filepath.Join(o.routeDir(rt), e.name))
	}
}

type queued struct {
	rt    route
	team  *teamRoute
	e     entry
	path  string
	size  int64
	mtime time.Time
}

// sweepOutbox enforces the bounds, oldest first, and returns the parts it removed per route
// so their senders stop queueing them.
func (r *Relay) sweepOutbox(now time.Time) map[route][]entry {
	o := r.outbox
	routes, _ := o.routes()
	var all []queued
	for _, rt := range routes {
		entries, _ := o.list(rt)
		for _, e := range entries {
			p := filepath.Join(o.routeDir(rt), e.name)
			info, err := os.Stat(p)
			if err != nil {
				continue
			}
			all = append(all, queued{rt: rt, e: e, path: p, size: info.Size(), mtime: info.ModTime()})
		}
	}
	teams, _ := o.teamRoutes()
	for _, rt := range teams {
		entries, _ := o.listTeam(rt)
		for _, e := range entries {
			p := filepath.Join(o.teamDir(rt), e.name)
			if info, err := os.Stat(p); err == nil {
				all = append(all, queued{team: &rt, e: e, path: p, size: info.Size(), mtime: info.ModTime()})
			}
		}
	}
	slices.SortFunc(all, func(a, b queued) int { return strings.Compare(a.e.name, b.e.name) })
	removed := map[route][]entry{}
	teamRemoved := map[teamRoute][]entry{}
	lost := map[string]int{}
	defer func() { r.recordLosses(lost, now) }()
	drop := func(q queued, why string) {
		if os.Remove(q.path) == nil {
			r.stats.dropped(q.e.signal, why, q.e.records)
			lost[why] += q.e.records
			if q.team != nil {
				teamRemoved[*q.team] = append(teamRemoved[*q.team], q.e)
				return
			}
			removed[q.rt] = append(removed[q.rt], q.e)
		}
	}
	var total int64
	var kept []queued
	for _, q := range all {
		if q.e.corrupt {
			drop(q, "outbox_unreadable")
			continue
		}
		age := q.mtime
		if at := q.e.received(); !at.IsZero() && at.Before(age) {
			age = at
		}
		if now.Sub(age) > maxOutboxAge {
			drop(q, "outbox_expired")
			continue
		}
		kept = append(kept, q)
		total += q.size
	}
	for _, q := range kept {
		if total <= maxOutboxBytes {
			break
		}
		drop(q, "outbox_full")
		total -= q.size
	}

	des, _ := os.ReadDir(filepath.Join(o.dir, deadDir))
	var dead []queued
	var deadTotal int64
	for _, de := range des {
		info, err := de.Info()
		if err != nil || !de.Type().IsRegular() {
			continue
		}
		dead = append(dead, queued{path: filepath.Join(o.dir, deadDir, de.Name()), size: info.Size(), mtime: info.ModTime()})
		deadTotal += info.Size()
	}
	slices.SortFunc(dead, func(a, b queued) int { return a.mtime.Compare(b.mtime) })
	for _, q := range dead {
		if deadTotal <= maxDeadBytes && now.Sub(q.mtime) <= maxOutboxAge {
			continue
		}
		_ = os.Remove(q.path)
		deadTotal -= q.size
	}
	for rt, gone := range teamRemoved {
		r.mu.Lock()
		s := r.teamSenders[rt]
		r.mu.Unlock()
		if s != nil {
			s.forget(gone)
		}
	}
	r.removeEmptyRoutes(routes, teams)
	return removed
}

// removeEmptyRoutes removes each route directory holding nothing but its identity, under deliverMu so no
// put is between creating one and writing its part; the next part recreates it. Under it,
// a temporary file is one a crashed put left.
func (r *Relay) removeEmptyRoutes(routes []route, teams []teamRoute) {
	r.deliverMu.Lock()
	defer r.deliverMu.Unlock()
	o := r.outbox
	for _, rt := range routes {
		dir := o.routeDir(rt)
		temps, _ := filepath.Glob(filepath.Join(dir, config.TempPrefix+"*"))
		for _, p := range temps {
			_ = os.Remove(p)
		}
		if left, err := os.ReadDir(dir); err != nil || len(left) > 1 || len(left) == 1 && left[0].Name() != identity {
			continue
		}
		_ = os.Remove(filepath.Join(dir, identity))
		// Each fails while anything is left in it.
		_ = os.Remove(dir)
		_ = os.Remove(filepath.Dir(dir))
		_ = os.Remove(filepath.Dir(filepath.Dir(dir)))
		if rt.account != "" {
			_ = os.Remove(filepath.Dir(filepath.Dir(filepath.Dir(dir))))
		}
	}
	for _, rt := range teams {
		pruneEmptyUp(o.teamDir(rt), 3)
	}
}

func mergeBodies(sig Signal, bodies [][]byte) ([]byte, error) {
	switch sig {
	case Logs:
		var out logspb.LogsData
		for _, b := range bodies {
			var m logspb.LogsData
			if err := proto.Unmarshal(b, &m); err != nil {
				return nil, err
			}
			out.ResourceLogs = append(out.ResourceLogs, m.ResourceLogs...)
		}
		return proto.Marshal(&out)
	case Traces:
		var out tracepb.TracesData
		for _, b := range bodies {
			var m tracepb.TracesData
			if err := proto.Unmarshal(b, &m); err != nil {
				return nil, err
			}
			out.ResourceSpans = append(out.ResourceSpans, m.ResourceSpans...)
		}
		return proto.Marshal(&out)
	default:
		var out metricspb.MetricsData
		for _, b := range bodies {
			var m metricspb.MetricsData
			if err := proto.Unmarshal(b, &m); err != nil {
				return nil, err
			}
			out.ResourceMetrics = append(out.ResourceMetrics, m.ResourceMetrics...)
		}
		return proto.Marshal(&out)
	}
}

// OutboxDir is the outbox's directory name under the relay directory. It is new in this
// version, so an older relay, which reads only its own, outbox, never sends what this one
// queued with its own credential; this one never reads an older relay's.
const OutboxDir = "outbox-v2"
