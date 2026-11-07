package sensor

import (
	"cmp"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"time"
)

// HostView is a session's traffic to one host, open connections included.
type HostView struct {
	Host  string `json:"host"`
	TX    uint64 `json:"tx"`
	RX    uint64 `json:"rx"`
	Flows int    `json:"flows"`
	Live  int    `json:"live"`
}

// FileView is a session's use of one file.
type FileView struct {
	Path string `json:"path"`
	FileUse
}

// SessionView is a session's totals now: closed connections plus the open ones' counters.
type SessionView struct {
	Root         int        `json:"root"`
	Agent        string     `json:"agent"`
	Start        time.Time  `json:"start"`
	End          time.Time  `json:"end,omitzero"`
	Procs        int        `json:"procs"`
	TX           uint64     `json:"tx"`
	RX           uint64     `json:"rx"`
	LiveFlows    int        `json:"live_flows"`
	FilesRead    uint64     `json:"files_read"`
	FilesWritten uint64     `json:"files_written"`
	Removed      int        `json:"removed"`
	Blocks       int        `json:"blocks"`
	Containers   int        `json:"containers"`
	Hosts        []HostView `json:"hosts"`
}

// Detail is one session in full.
type Detail struct {
	SessionView
	Flows      []Flow      `json:"flows"`
	Files      []FileView  `json:"files"`
	Ops        []FileOp    `json:"ops"`
	BlockList  []Block     `json:"block_list"`
	Containers []Container `json:"container_list"`
	Pulls      []ImagePull `json:"pulls"`
	Execs      []Proc      `json:"execs"`
}

// Views summarises every session.
func (s *State) Views() []SessionView {
	live, files := s.backend.Live(), s.backend.LiveFiles()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SessionView, 0, len(s.sessions))
	for _, ss := range s.sessions {
		v := s.viewLocked(ss, live)
		for _, f := range files {
			if f.Root == ss.Root {
				v.FilesRead += f.Read
				v.FilesWritten += f.Written
			}
		}
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b SessionView) int { return b.Start.Compare(a.Start) })
	return out
}

// Detail is one session, or false.
func (s *State) Detail(root int) (Detail, bool) {
	live, files := s.backend.Live(), s.backend.LiveFiles()
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.sessions[root]
	if ss == nil {
		return Detail{}, false
	}
	d := Detail{SessionView: s.viewLocked(ss, live), Containers: ss.Containers, Pulls: ss.Pulls}
	for _, f := range live {
		if f.Root == root {
			f.Host = s.hostLocked(f)
			d.Flows = append(d.Flows, f)
		}
	}
	slices.SortFunc(d.Flows, func(a, b Flow) int { return cmp.Compare(b.TX+b.RX, a.TX+a.RX) })
	merged := map[string]FileUse{}
	for path, u := range ss.Files {
		merged[path] = *u
	}
	for _, f := range files {
		if f.Root == root {
			u := merged[f.Path]
			u.Read, u.Written, u.Open = u.Read+f.Read, u.Written+f.Written, u.Open+1
			merged[f.Path] = u
			d.FilesRead, d.FilesWritten = d.FilesRead+f.Read, d.FilesWritten+f.Written
		}
	}
	for path, u := range merged {
		d.Files = append(d.Files, FileView{Path: path, FileUse: u})
	}
	slices.SortFunc(d.Files, func(a, b FileView) int {
		return cmp.Compare(b.Read+b.Written, a.Read+a.Written)
	})
	d.Files = d.Files[:min(len(d.Files), 100)]
	d.Ops = tail(ss.Ops, 100)
	d.BlockList = tail(ss.Blocks, 100)
	d.Execs = tail(ss.Execs, 100)
	return d, true
}

func (s *State) viewLocked(ss *Session, live []Flow) SessionView {
	v := SessionView{
		Root: ss.Root, Agent: ss.Agent, Start: ss.Start, End: ss.End, Procs: ss.Procs,
		Blocks: len(ss.Blocks), Containers: len(ss.Containers),
	}
	hosts := map[string]*HostView{}
	host := func(h string) *HostView {
		if hosts[h] == nil {
			hosts[h] = &HostView{Host: h}
		}
		return hosts[h]
	}
	for h, t := range ss.Net {
		hv := host(h)
		hv.TX, hv.RX, hv.Flows = hv.TX+t.TX, hv.RX+t.RX, hv.Flows+t.Flows
	}
	for _, f := range live {
		if f.Root != ss.Root {
			continue
		}
		hv := host(s.hostLocked(f))
		hv.TX, hv.RX, hv.Live = hv.TX+f.TX, hv.RX+f.RX, hv.Live+1
		v.LiveFlows++
	}
	for _, hv := range hosts {
		v.TX, v.RX = v.TX+hv.TX, v.RX+hv.RX
		v.Hosts = append(v.Hosts, *hv)
	}
	slices.SortFunc(v.Hosts, func(a, b HostView) int { return cmp.Compare(b.TX+b.RX, a.TX+a.RX) })
	v.Hosts = v.Hosts[:min(len(v.Hosts), 10)]
	for _, u := range ss.Files {
		v.FilesRead += u.Read
		v.FilesWritten += u.Written
		if u.Removed {
			v.Removed++
		}
	}
	return v
}

func tail[T any](s []T, n int) []T { return slices.Clone(s[max(0, len(s)-n):]) }

// Serve answers the query API on a unix socket at path, readable by its owner alone.
func (s *State) Serve(ctx context.Context, path string) error {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.Views())
	})
	mux.HandleFunc("GET /v1/sessions/{root}", func(w http.ResponseWriter, r *http.Request) {
		root, _ := strconv.Atoi(r.PathValue("root"))
		d, ok := s.Detail(root)
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, d)
	})
	mux.HandleFunc("POST /v1/watch/{pid}", func(w http.ResponseWriter, r *http.Request) {
		pid, _ := strconv.Atoi(r.PathValue("pid"))
		if err := s.Adopt(Proc{PID: pid, Exe: exeOf(pid), Start: time.Now()}); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
	})
	mux.HandleFunc("PUT /v1/policy", func(w http.ResponseWriter, r *http.Request) {
		var p Policy
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.SetPolicy(r.Context(), p); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func exeOf(pid int) string {
	exe, _ := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	return exe
}
