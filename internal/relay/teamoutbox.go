package relay

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/project"
)

// Routed parts live under <dir>/.teams/@<account>/<enrollment>/<tool>/<name>.pb.
const teamsDir, maxHeader = ".teams", 64 << 10

var errCorruptHeader = errors.New("corrupt team header")

type teamRoute struct{ account, enrollment, tool string }

func (rt teamRoute) toolLabel() string { return strings.TrimPrefix(rt.tool, noTool) }
func (rt teamRoute) String() string {
	return teamsDir + "/" + accountPrefix + rt.account + "/" + rt.enrollment + "/" + rt.tool
}
func validTeamRoute(rt teamRoute) bool {
	return rt.account != "" && validAccount(rt.account) && project.ValidID(rt.enrollment) && (rt.tool == noTool || project.ValidID(rt.tool))
}
func (o outbox) teamDir(rt teamRoute) string {
	return filepath.Join(o.dir, teamsDir, accountPrefix+rt.account, rt.enrollment, rt.tool)
}
func (o outbox) putTeam(rt teamRoute, q Queued, e entry, body []byte) error {
	if o.dir == "" || !validTeamRoute(rt) {
		return fmt.Errorf("unusable route %q", rt)
	}
	header, err := json.Marshal(q)
	if err == nil {
		err = os.MkdirAll(o.teamDir(rt), 0o700)
	}
	if err != nil {
		return err
	}
	return config.WriteFileAtomicNoSync(filepath.Join(o.teamDir(rt), e.name), append(append(append(make([]byte, 0, len(header)+1+len(body)), header...), '\n'), body...), 0o600)
}
func readTeamHeader(path, account string) (Queued, error) {
	f, err := os.Open(path)
	if err != nil {
		return Queued{}, err
	}
	line, rerr := bufio.NewReader(io.LimitReader(f, maxHeader)).ReadBytes('\n')
	_ = f.Close()
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		return Queued{}, rerr
	}
	var q Queued
	if rerr != nil || json.Unmarshal(line, &q) != nil {
		return Queued{}, errCorruptHeader
	}
	q.Account = cmp.Or(q.Account, account)
	return q, nil
}
func (o outbox) listTeam(rt teamRoute) ([]entry, error) {
	des, err := readSubdirs(o.teamDir(rt))
	var out []entry
	for _, de := range des {
		if e, ok := parseEntry(de.Name()); ok && de.Type().IsRegular() {
			switch q, rerr := readTeamHeader(filepath.Join(o.teamDir(rt), e.name), rt.account); {
			case errors.Is(rerr, fs.ErrNotExist):
				continue
			case errors.Is(rerr, errCorruptHeader):
				e.corrupt = true
			case rerr != nil:
				e.unread, err = true, errors.Join(err, rerr)
			default:
				e.q = q
			}
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b entry) int { return strings.Compare(a.name, b.name) })
	return out, err
}

// QueuedSessions lists session/subagent IDs queued under dir/.teams/ for runs.Prune.
func QueuedSessions(dir string) (map[string]bool, error) {
	o, out := outbox{dir: dir}, map[string]bool{}
	routes, err := o.teamRoutes()
	for _, rt := range routes {
		entries, lerr := o.listTeam(rt)
		err = errors.Join(err, lerr)
		for _, e := range entries {
			if !e.corrupt && !e.unread && e.q.Session != "" {
				out[e.q.Session] = true
			}
		}
	}
	return out, err
}

// QueuedSessions lists session/subagent IDs queued in this relay's .teams/ outbox.
func (r *Relay) QueuedSessions() (map[string]bool, error) { return QueuedSessions(r.outbox.dir) }
func readSubdirs(dir string) ([]os.DirEntry, error) {
	if des, err := os.ReadDir(dir); !errors.Is(err, fs.ErrNotExist) {
		return des, err
	}
	return nil, nil
}
func (o outbox) teamRoutes() ([]teamRoute, error) {
	root := filepath.Join(o.dir, teamsDir)
	accounts, err := readSubdirs(root)
	var out []teamRoute
	for _, a := range accounts {
		if account, ok := strings.CutPrefix(a.Name(), accountPrefix); ok && a.IsDir() && validAccount(account) && account != "" {
			enrollments, eerr := readSubdirs(filepath.Join(root, a.Name()))
			err = errors.Join(err, eerr)
			for _, en := range enrollments {
				if en.IsDir() {
					tools, terr := readSubdirs(filepath.Join(root, a.Name(), en.Name()))
					err = errors.Join(err, terr)
					for _, t := range tools {
						if rt := (teamRoute{account, en.Name(), t.Name()}); t.IsDir() && validTeamRoute(rt) {
							out = append(out, rt)
						}
					}
				}
			}
		}
	}
	return out, err
}
func pruneEmptyUp(dir string, levels int) {
	tmp, _ := filepath.Glob(filepath.Join(dir, config.TempPrefix+"*"))
	for _, p := range tmp {
		_ = os.Remove(p)
	}
	for range levels {
		_ = os.Remove(dir)
		dir = filepath.Dir(dir)
	}
}

// PurgeOldOutbox deletes pre-cutover parts outside .teams/ under dir, leaves .teams/ untouched,
// and records dropped records in losses.json under "no_destination".
func PurgeOldOutbox(dir string, now time.Time) (int, error) {
	if dir == "" {
		return 0, nil
	}
	o, dropped := outbox{dir: dir}, 0
	routes, err := o.routes()
	for _, rt := range routes {
		entries, _ := o.list(rt)
		d := o.routeDir(rt)
		for _, e := range entries {
			if config.Remove(filepath.Join(d, e.name)) == nil {
				dropped += e.records
			}
		}
		_ = os.Remove(filepath.Join(d, identity))
		pruneEmptyUp(d, map[bool]int{true: 4, false: 3}[rt.account != ""])
	}
	if dropped > 0 {
		recordLossesDir(dir, map[string]int{lostNoDestination: dropped}, now)
	}
	return dropped, err
}
