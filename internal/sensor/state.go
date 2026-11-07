package sensor

import (
	"context"
	"log/slog"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Session is what one watched root and everything it started did.
type Session struct {
	Root       int                 `json:"root"`
	Agent      string              `json:"agent"`
	Args       []string            `json:"args,omitempty"`
	Start      time.Time           `json:"start"`
	End        time.Time           `json:"end,omitzero"`
	Procs      int                 `json:"procs"`
	Net        map[string]*Traffic `json:"net"`
	Files      map[string]*FileUse `json:"files"`
	Ops        []FileOp            `json:"ops,omitempty"`
	Blocks     []Block             `json:"blocks,omitempty"`
	Containers []Container         `json:"containers,omitempty"`
	Pulls      []ImagePull         `json:"pulls,omitempty"`
	Execs      []Proc              `json:"execs,omitempty"`
}

// Traffic is a session's closed connections to one host.
type Traffic struct {
	TX    uint64 `json:"tx"`
	RX    uint64 `json:"rx"`
	Flows int    `json:"flows"`
}

// FileUse is what a session did to one file.
type FileUse struct {
	Read    uint64 `json:"read"`
	Written uint64 `json:"written"`
	Opens   int    `json:"opens"`
	Open    int    `json:"open,omitempty"` // still open now, counted so far
	Removed bool   `json:"removed,omitempty"`
	Renamed string `json:"renamed,omitempty"`
}

// Container is a container a session started, counted with it from then on.
type Container struct {
	ID     string    `json:"id"`
	Name   string    `json:"name"`
	Image  string    `json:"image"`
	Cgroup uint64    `json:"cgroup"`
	Start  time.Time `json:"start"`
	Via    string    `json:"via"` // the command that started it
}

const (
	keepOps   = 500
	keepExecs = 200
)

// State keeps the watched sessions. It is the backend's Sink.
type State struct {
	backend Backend
	exes    map[string]bool // root executables, by base name
	log     *slog.Logger

	// lends is whether the backend can lend the container engine to a session.
	lends bool

	mu       sync.Mutex
	sessions map[int]*Session
	lent     map[int]int // a running container command -> the session the engine is lent to
	names    map[netip.Addr]string
	hellos   map[[2]netip.AddrPort]string
	policy   Policy
}

// NewState watches every process whose executable's base name is one of exes.
func NewState(b Backend, exes []string, log *slog.Logger) *State {
	s := &State{
		backend: b, exes: map[string]bool{}, log: log,
		sessions: map[int]*Session{}, names: map[netip.Addr]string{}, hellos: map[[2]netip.AddrPort]string{},
		lent: map[int]int{},
	}
	for _, e := range exes {
		s.exes[e] = true
	}
	if err := b.Roots(exes); err != nil {
		log.Warn("roots", "err", err)
	}
	return s
}

// Exec starts a session for a root executable, and records a watched process's command.
func (s *State) Exec(p Proc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case p.Root == p.PID && s.sessions[p.PID] == nil: // the kernel made it a root at exec
	case p.Root != 0:
		if ss := s.sessions[p.Root]; ss != nil {
			ss.Execs = appendCapped(ss.Execs, p, keepExecs)
		}
		s.lendLocked(p)
		return
	case s.exes[filepath.Base(p.Exe)]: // a backend that cannot start roots itself
		if err := s.backend.Watch(p.PID); err != nil {
			s.log.Warn("watch", "pid", p.PID, "err", err)
			return
		}
	default:
		return
	}
	s.sessions[p.PID] = newSession(p)
	s.log.Info("session", "root", p.PID, "agent", p.Exe, "args", p.Args)
}

// LendEngine has the state lend the container engine to a session while one of its
// container commands runs.
func (s *State) LendEngine() { s.lends = true }

// lendLocked lends the engine to p's session while p, a command that has the engine pull,
// build or start for it, runs. Another session's work in the engine meanwhile is counted
// with it too.
func (s *State) lendLocked(p Proc) {
	if !s.lends || !slices.Contains(containerCLIs, filepath.Base(p.Exe)) || !engineWork(p.Args) {
		return
	}
	if err := s.backend.Lend(p.Root); err != nil {
		s.log.Warn("lend", "err", err)
		return
	}
	s.lent[p.PID] = p.Root
	s.log.Info("engine lent", "root", p.Root, "for", strings.Join(p.Args, " "))
}

// engineWork is whether a container command's arguments have the engine fetch or build.
func engineWork(args []string) bool {
	for _, a := range args {
		switch a {
		case "pull", "run", "create", "up", "build", "start", "buildx":
			return true
		}
	}
	return false
}

// Adopt watches an already running process as a root, as Exec would have.
func (s *State) Adopt(p Proc) error {
	if err := s.backend.Watch(p.PID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[p.PID] == nil {
		s.sessions[p.PID] = newSession(p)
	}
	return nil
}

func newSession(p Proc) *Session {
	return &Session{
		Root: p.PID, Agent: p.Exe, Args: p.Args, Start: p.Start, Procs: 1,
		Net: map[string]*Traffic{}, Files: map[string]*FileUse{},
	}
}

// Fork counts a process a watched one started.
func (s *State) Fork(p Proc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ss := s.sessions[p.Root]; ss != nil {
		ss.Procs++
	}
}

// Exit ends a session when its root exits, and the engine's loan when a container command does.
func (s *State) Exit(pid, root int, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.lent[pid]; ok {
		delete(s.lent, pid)
		still := false
		for _, other := range s.lent {
			still = still || other == r
		}
		if !still {
			if err := s.backend.Unlend(r); err != nil {
				s.log.Warn("unlend", "err", err)
			}
		}
	}
	if ss := s.sessions[root]; ss != nil && pid == root {
		ss.End = at
	}
}

// FlowClosed adds a closed connection's bytes to its session, under the host it reached.
func (s *State) FlowClosed(f Flow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.sessions[f.Root]
	if ss == nil {
		return
	}
	host := s.hostLocked(f)
	t := ss.Net[host]
	if t == nil {
		t = &Traffic{}
		ss.Net[host] = t
	}
	t.TX += f.TX
	t.RX += f.RX
	t.Flows++
}

// File records what a watched process did to a file.
func (s *State) File(op FileOp) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.sessions[op.Root]
	if ss == nil {
		return
	}
	u := ss.Files[op.Path]
	if u == nil {
		u = &FileUse{}
		ss.Files[op.Path] = u
	}
	switch op.Op {
	case OpClose:
		u.Read += op.Read
		u.Written += op.Written
		u.Opens++
		if op.Written > 0 {
			ss.Ops = appendCapped(ss.Ops, op, keepOps)
		}
	case OpUnlink:
		u.Removed = true
		ss.Ops = appendCapped(ss.Ops, op, keepOps)
	case OpRename:
		u.Renamed = op.To
		ss.Ops = appendCapped(ss.Ops, op, keepOps)
	}
}

// Blocked records a policy's denial, or audit-mode near miss.
func (s *State) Blocked(b Block) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ss := s.sessions[b.Root]; ss != nil {
		ss.Blocks = appendCapped(ss.Blocks, b, keepOps)
	}
	s.log.Info("policy", "kind", b.Kind, "target", b.Target, "rule", b.Rule, "enforced", b.Enforced, "comm", b.Comm)
}

// Name learns that addr answers for host, and re-applies a policy that denies host.
func (s *State) Name(addr netip.Addr, host string) {
	s.mu.Lock()
	s.names[addr.Unmap()] = host
	_, denied := ruleFor(s.policy, host)
	p := s.policy
	s.mu.Unlock()
	if denied {
		if err := s.applyPolicy(context.Background(), p); err != nil {
			s.log.Warn("policy", "err", err)
		}
	}
}

// Hello learns the host a connection's TLS or QUIC hello named.
func (s *State) Hello(local, remote netip.AddrPort, host string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hellos[[2]netip.AddrPort{unmap(local), unmap(remote)}] = host
	s.names[remote.Addr().Unmap()] = host
}

// SetPolicy resolves p and hands it to the backend.
func (s *State) SetPolicy(ctx context.Context, p Policy) error {
	s.mu.Lock()
	s.policy = p
	s.mu.Unlock()
	return s.applyPolicy(ctx, p)
}

func (s *State) applyPolicy(ctx context.Context, p Policy) error {
	s.mu.Lock()
	seen := make(map[netip.Addr]string, len(s.names))
	for a, h := range s.names {
		seen[a] = h
	}
	s.mu.Unlock()
	return s.backend.Enforce(resolve(ctx, p, seen))
}

// AddContainer counts a container's processes toward root's session.
func (s *State) AddContainer(root int, c Container) error {
	if c.Cgroup != 0 {
		if err := s.backend.WatchCgroup(c.Cgroup, root); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ss := s.sessions[root]; ss != nil {
		ss.Containers = append(ss.Containers, c)
	}
	s.log.Info("container", "root", root, "name", c.Name, "image", c.Image, "via", c.Via)
	return nil
}

// AddPull records an image a session had pulled.
func (s *State) AddPull(root int, p ImagePull) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ss := s.sessions[root]; ss != nil {
		ss.Pulls = append(ss.Pulls, p)
	}
	s.log.Info("image pull", "root", root, "image", p.Image, "size", p.Size, "via", p.Via)
}

// RecentExecs is what each live session ran since since, newest last.
func (s *State) RecentExecs(since time.Time) []Proc {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Proc
	for _, ss := range s.sessions {
		for _, p := range ss.Execs {
			if p.Start.After(since) {
				out = append(out, p)
			}
		}
	}
	slices.SortFunc(out, func(a, b Proc) int { return a.Start.Compare(b.Start) })
	return out
}

func (s *State) hostLocked(f Flow) string {
	if h := s.hellos[[2]netip.AddrPort{unmap(f.Local), unmap(f.Remote)}]; h != "" {
		return h
	}
	if h := s.names[f.Remote.Addr().Unmap()]; h != "" {
		return h
	}
	return f.Remote.Addr().String()
}

func unmap(ap netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

func appendCapped[T any](s []T, v T, limit int) []T {
	s = append(s, v)
	if len(s) > limit {
		s = s[len(s)-limit:]
	}
	return s
}
