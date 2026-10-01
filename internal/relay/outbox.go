package relay

import (
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

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// The outbox is plain files, one part each, renamed into place so a crash leaves a whole file or none:
//
//	<dir>/<project>/<tool>/<received>-<seq>-<signal>-<records>.pb   accepted, not yet delivered
//	<dir>/.dead/<project>-<tool>-<name>                             refused by the host
//
// <received> is zero-padded Unix nanoseconds, so a name sort is arrival order. Only claimed
// parts, already filtered, are written; unsynced, so a power cut can lose the last seconds.

const (
	deadDir  = ".dead"
	noTool   = "_"
	pbSuffix = ".pb"
)

// A machine offline for weeks, or a key refused for good, would otherwise fill the disk.
const (
	maxOutboxBytes = 256 << 20
	maxDeadBytes   = 32 << 20
	maxOutboxAge   = 14 * 24 * time.Hour
)

// route is where a part goes: a project, with the key of the tool that claimed it.
type route struct {
	project string
	tool    string
}

func routeOf(c claim.Claim) route {
	t := c.Tool
	if t == "" {
		t = noTool
	}
	return route{project: c.ProjectID, tool: t}
}

func (rt route) toolLabel() string {
	if rt.tool == noTool {
		return ""
	}
	return rt.tool
}

func (rt route) String() string { return rt.project + "/" + rt.tool }

type entry struct {
	name    string
	signal  Signal
	records int
}

var seq atomic.Uint64

func newEntry(now time.Time, sig Signal, records int) entry {
	n := seq.Add(1) % 1_000_000
	return entry{name: fmt.Sprintf("%020d-%06d-%s-%d%s", now.UnixNano(), n, sig, records, pbSuffix), signal: sig, records: records}
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

func (o outbox) routeDir(rt route) string { return filepath.Join(o.dir, rt.project, rt.tool) }

// validRoute admits a route's two directory names: a project id and a tool label.
func validRoute(rt route) bool {
	return project.ValidID(rt.project) && (rt.tool == noTool || project.ValidID(rt.tool))
}

func (o outbox) put(rt route, e entry, body []byte) error {
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

func (o outbox) read(rt route, e entry) ([]byte, error) {
	return os.ReadFile(filepath.Join(o.routeDir(rt), e.name))
}

func (o outbox) remove(rt route, batch []entry) {
	for _, e := range batch {
		_ = os.Remove(filepath.Join(o.routeDir(rt), e.name))
	}
}

func (o outbox) bury(rt route, e entry) {
	dead := filepath.Join(o.dir, deadDir)
	src := filepath.Join(o.routeDir(rt), e.name)
	if os.MkdirAll(dead, 0o700) != nil || os.Rename(src, filepath.Join(dead, rt.project+"-"+rt.tool+"-"+e.name)) != nil {
		_ = os.Remove(src)
	}
}

func (o outbox) routes() ([]route, error) {
	projects, err := os.ReadDir(o.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []route
	for _, p := range projects {
		if !p.IsDir() || strings.HasPrefix(p.Name(), ".") {
			continue
		}
		tools, err := os.ReadDir(filepath.Join(o.dir, p.Name()))
		if err != nil {
			continue
		}
		for _, t := range tools {
			rt := route{project: p.Name(), tool: t.Name()}
			if t.IsDir() && validRoute(rt) {
				out = append(out, rt)
			}
		}
	}
	return out, nil
}

type queued struct {
	rt    route
	e     entry
	path  string
	size  int64
	mtime time.Time
}

// sweepOutbox enforces the bounds, oldest first, and returns how many parts it removed per
// route so their senders' counts stay right.
func (r *Relay) sweepOutbox(now time.Time) map[route]int {
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
			all = append(all, queued{rt, e, p, info.Size(), info.ModTime()})
		}
	}
	slices.SortFunc(all, func(a, b queued) int { return strings.Compare(a.e.name, b.e.name) })
	removed := map[route]int{}
	drop := func(q queued, why string) {
		if os.Remove(q.path) == nil {
			r.stats.dropped(q.e.signal, why, q.e.records)
			removed[q.rt]++
		}
	}
	var total int64
	var kept []queued
	for _, q := range all {
		if now.Sub(q.mtime) > maxOutboxAge {
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
	return removed
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

// OutboxDir is the outbox's directory name under the relay directory.
const OutboxDir = "outbox"

// Queued is what waits in the outbox for one route.
type Queued struct {
	Project string
	Tool    string
	Parts   int
	Records int
}

// Backlog lists what waits in the outbox at dir, by route.
func Backlog(dir string) []Queued {
	o := outbox{dir}
	routes, _ := o.routes()
	var out []Queued
	for _, rt := range routes {
		entries, _ := o.list(rt)
		if len(entries) == 0 {
			continue
		}
		q := Queued{Project: rt.project, Tool: rt.toolLabel(), Parts: len(entries)}
		for _, e := range entries {
			q.Records += e.records
		}
		out = append(out, q)
	}
	return out
}
