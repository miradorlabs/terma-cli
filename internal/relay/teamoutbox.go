package relay

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/project"
)

// On a migrated machine the outbox queues by enrollment, beside the single-team routes:
//
//	<dir>/.teams/<enrollment>/<tool>/<received>-<seq>-<signal>-<records>.pb
//
// Each file is one part, renamed into place: a line of JSON, the part's destination
// (Queued: its enrollment, location, run, record time and whether the location is its
// own), then the part as it was accepted. The folder starts with a dot, so a relay from
// before the migration, which reads only project folders, never sends what is queued here.

const (
	teamsDir = ".teams"
	// maxHeader bounds a queued part's first line.
	maxHeader = 64 << 10
)

// teamRoute is an enrollment's queue for the parts one tool claimed.
type teamRoute struct {
	enrollment string
	tool       string
}

func (rt teamRoute) toolLabel() string {
	if rt.tool == noTool {
		return ""
	}
	return rt.tool
}

func (rt teamRoute) String() string { return teamsDir + "/" + rt.enrollment + "/" + rt.tool }

func validTeamRoute(rt teamRoute) bool {
	return project.ValidID(rt.enrollment) && (rt.tool == noTool || project.ValidID(rt.tool))
}

// teamEntry is a queued part and where it goes.
type teamEntry struct {
	entry
	q Queued
}

func (o outbox) teamDir(rt teamRoute) string {
	return filepath.Join(o.dir, teamsDir, rt.enrollment, rt.tool)
}

// putTeam writes a part with its destination.
func (o outbox) putTeam(rt teamRoute, q Queued, e entry, body []byte) error {
	if o.dir == "" {
		return errors.New("the relay has no outbox directory")
	}
	if !validTeamRoute(rt) {
		return fmt.Errorf("unusable route %q", rt)
	}
	header, err := json.Marshal(q)
	if err != nil {
		return err
	}
	dir := o.teamDir(rt)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data := make([]byte, 0, len(header)+1+len(body))
	data = append(append(append(data, header...), '\n'), body...)
	return config.WriteFileAtomicNoSync(filepath.Join(dir, e.name), data, 0o600)
}

// listTeam is a route's parts in arrival order, each with its destination; one whose
// destination cannot be read is left for the janitor's age bound, never sent.
func (o outbox) listTeam(rt teamRoute) ([]teamEntry, error) {
	des, err := os.ReadDir(o.teamDir(rt))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []teamEntry
	for _, de := range des {
		if !de.Type().IsRegular() {
			continue
		}
		e, ok := parseEntry(de.Name())
		if !ok {
			continue
		}
		q, err := o.header(rt, e)
		if err != nil {
			continue
		}
		out = append(out, teamEntry{e, q})
	}
	slices.SortFunc(out, func(a, b teamEntry) int { return strings.Compare(a.name, b.name) })
	return out, nil
}

// teamNames is a route's parts by name alone, for the janitor.
func (o outbox) teamNames(rt teamRoute) []entry {
	des, _ := os.ReadDir(o.teamDir(rt))
	var out []entry
	for _, de := range des {
		if e, ok := parseEntry(de.Name()); ok && de.Type().IsRegular() {
			out = append(out, e)
		}
	}
	return out
}

// QueuedSessions are the session and subagent IDs the parts in the outbox name, which the
// run ledger keeps (runs.Prune) while any is queued; an error when the outbox cannot be
// listed. A part whose destination cannot be read is never sent, and names none. Parts
// queued before the migration name no run.
func (r *Relay) QueuedSessions() (map[string]bool, error) {
	routes, err := r.outbox.teamRoutes()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, rt := range routes {
		for _, e := range r.outbox.teamNames(rt) {
			if q, err := r.outbox.header(rt, e); err == nil && q.Session != "" {
				out[q.Session] = true
			}
		}
	}
	return out, nil
}

// header reads a queued part's destination alone.
func (o outbox) header(rt teamRoute, e entry) (Queued, error) {
	f, err := os.Open(filepath.Join(o.teamDir(rt), e.name))
	if err != nil {
		return Queued{}, err
	}
	defer func() { _ = f.Close() }()
	line, err := bufio.NewReader(io.LimitReader(f, maxHeader)).ReadBytes('\n')
	if err != nil {
		return Queued{}, err
	}
	var q Queued
	return q, json.Unmarshal(line, &q)
}

// readTeam is a queued part's body, without its destination.
func (o outbox) readTeam(rt teamRoute, e entry) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(o.teamDir(rt), e.name))
	if err != nil {
		return nil, err
	}
	_, body, ok := bytes.Cut(data, []byte("\n"))
	if !ok {
		return nil, errors.New("a queued part without its destination")
	}
	return body, nil
}

func (o outbox) removeTeam(rt teamRoute, batch []entry) {
	for _, e := range batch {
		_ = config.Remove(filepath.Join(o.teamDir(rt), e.name))
	}
}

func (o outbox) buryTeam(rt teamRoute, e entry) {
	dead := filepath.Join(o.dir, deadDir)
	src := filepath.Join(o.teamDir(rt), e.name)
	if os.MkdirAll(dead, 0o700) != nil || config.Rename(src, filepath.Join(dead, "team-"+rt.enrollment+"-"+rt.tool+"-"+e.name)) != nil {
		_ = config.Remove(src)
	}
}

// teamRoutes lists every enrollment's queues.
func (o outbox) teamRoutes() ([]teamRoute, error) {
	enrollments, err := os.ReadDir(filepath.Join(o.dir, teamsDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []teamRoute
	for _, en := range enrollments {
		if !en.IsDir() {
			continue
		}
		tools, err := os.ReadDir(filepath.Join(o.dir, teamsDir, en.Name()))
		if err != nil {
			continue
		}
		for _, t := range tools {
			rt := teamRoute{enrollment: en.Name(), tool: t.Name()}
			if t.IsDir() && validTeamRoute(rt) {
				out = append(out, rt)
			}
		}
	}
	return out, nil
}

// removeEmptyTeamRoutes removes each enrollment queue holding nothing, under deliverMu as
// removeEmptyRoutes does; a temporary file under it is one a crashed put left.
func (r *Relay) removeEmptyTeamRoutes(routes []teamRoute) {
	r.deliverMu.Lock()
	defer r.deliverMu.Unlock()
	for _, rt := range routes {
		dir := r.outbox.teamDir(rt)
		temps, _ := filepath.Glob(filepath.Join(dir, config.TempPrefix+"*"))
		for _, p := range temps {
			_ = os.Remove(p)
		}
		// Each fails while anything is left in it.
		_ = os.Remove(dir)
		_ = os.Remove(filepath.Dir(dir))
	}
}
