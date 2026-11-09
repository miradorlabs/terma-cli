package e2e

import (
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// What a trace saw besides paths and programs: socket calls (where the build listened and
// connected, by role), what only Endpoint Security sees, and the decoys the build touched.

// socketEvent records a socket call.
func (s TraceScope) socketEvent(out *FilesProbe, ev TraceEvent, phase, program string, dns map[string]bool) {
	if ev.Kind == "dns" {
		if ev.Name != "" {
			dns[strings.TrimSuffix(strings.ToLower(ev.Name), ".")] = true
		}
		return
	}
	sock := ev.Sock
	if sock == nil {
		return
	}
	use := func(m map[string]*SockUse, key, role string) *SockUse {
		u := m[key]
		if u == nil {
			u = &SockUse{Role: role, Missing: true}
			m[key] = u
		}
		u.Count++
		u.Phases = addWord(u.Phases, phase)
		if program != "" {
			u.Programs = addWord(u.Programs, program)
		}
		return u
	}
	if sock.Proto == "unix" {
		path := sock.Host
		if path == "" {
			return
		}
		key := path
		if !strings.HasPrefix(path, "@") {
			key = s.normalize(path)
		}
		u := use(out.Sockets.Unix, key, s.unixRole(path))
		u.Ops = addWord(u.Ops, ev.Kind)
		if ev.Err != "ENOENT" && ev.Err != "ECONNREFUSED" {
			u.Missing = false
		}
		if ev.Kind == "connect" && ev.Err == "" && slices.Contains(s.Decoys, path) {
			raiseDecoy(out, s.normalize(path), "connected")
		}
		return
	}
	switch ev.Kind {
	case "connect":
		if sock.Host == "" || sock.Port == 0 && sock.Proto == "udp" {
			return
		}
		to, role := s.endpoint(sock.Host, sock.Port)
		u := use(out.Sockets.Connections, sock.Proto+" "+to, role)
		if ev.Err != "ENOENT" && ev.Err != "ECONNREFUSED" && ev.Err != "ENETUNREACH" {
			u.Missing = false
		}
	case "bind", "listen":
		// A client's own end (a UDP socket bound to any port to send from) listens for nothing.
		if ev.Kind == "bind" && (sock.Proto == "tcp" || sock.Ephemeral || sock.Port == 0) {
			return
		}
		port := strconv.Itoa(sock.Port)
		if sock.Ephemeral {
			port = "<ephemeral>"
		}
		host := sock.Host
		if host == "" {
			host = "*"
		}
		u := use(out.Sockets.Listening, sock.Proto+" "+net.JoinHostPort(host, port), "")
		u.Missing = false
	}
}

// endpoint names a socket address: the run's own by their names, the resolver, a loopback
// address as <local>, any other as <remote>, each with its port.
func (s TraceScope) endpoint(host string, port int) (string, string) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	if name, ok := s.Endpoints[addr]; ok {
		return name, strings.Trim(name, "<>")
	}
	ip := net.ParseIP(host)
	switch {
	case port == 53:
		return "<resolver>:53", "dns"
	case ip != nil && ip.IsLoopback():
		return "<local>:" + strconv.Itoa(port), ""
	}
	return "<remote>:" + strconv.Itoa(port), ""
}

// unixRole says what a Unix socket is, where its path says: the planted decoys, an SSH agent,
// Docker, D-Bus, X11, Wayland, the resolver cache, the system log.
func (s TraceScope) unixRole(path string) string {
	base := filepath.Base(path)
	switch {
	case slices.Contains(s.Decoys, path) && strings.Contains(path, "docker"):
		return "decoy docker"
	case slices.Contains(s.Decoys, path):
		return "decoy ssh-agent"
	case strings.Contains(path, "ssh-agent") || strings.Contains(path, "agent.") && strings.Contains(path, "ssh") || strings.Contains(path, "com.apple.launchd") && strings.HasSuffix(path, "Listeners"):
		return "ssh-agent"
	case strings.Contains(base, "docker") || strings.Contains(path, "/.docker/") || strings.Contains(path, "podman"):
		return "docker"
	case strings.Contains(path, "dbus") || base == "bus":
		return "dbus"
	case strings.Contains(path, ".X11-unix"):
		return "x11"
	case strings.HasPrefix(base, "wayland-"):
		return "wayland"
	case strings.Contains(path, "nscd"):
		return "nscd"
	case path == "/dev/log" || strings.Contains(path, "journal") || strings.Contains(path, "syslog"):
		return "syslog"
	case strings.HasPrefix(path, "@"):
		return "abstract"
	}
	return ""
}

// macEvent records what only Endpoint Security sees.
func (s TraceScope) macEvent(out *FilesProbe, ev TraceEvent, phase, program string) {
	if out.MacOS == nil {
		out.MacOS = &MacOSUse{XPC: map[string]*SockUse{}, LaunchItems: map[string]*SockUse{}, TCC: map[string]*SockUse{}, TaskPorts: map[string]*SockUse{}}
	}
	m := map[string]map[string]*SockUse{"xpc": out.MacOS.XPC, "launch-item": out.MacOS.LaunchItems, "tcc": out.MacOS.TCC, "task-port": out.MacOS.TaskPorts}[ev.Kind]
	name := ev.Name
	switch {
	case ev.Kind == "task-port" && program != "":
		// A helper that reads every process (ps) does so by its nature: one entry, and no list
		// of what the machine runs.
		name = "<processes>"
	case ev.Kind == "task-port":
		name = filepath.Base(name) // the process, not where this machine installed it
	case strings.HasPrefix(name, "/"):
		name = s.normalize(name)
	}
	if name == "" || name == "." {
		return
	}
	u := m[name]
	if u == nil {
		u = &SockUse{}
		m[name] = u
	}
	u.Count++
	u.Phases = addWord(u.Phases, phase)
	if program != "" {
		u.Programs = addWord(u.Programs, program)
	}
}

// decoyEvent raises a decoy's level with what ev did to it.
func (s TraceScope) decoyEvent(out *FilesProbe, ev TraceEvent, op string) {
	if !slices.Contains(s.Decoys, ev.Path) {
		return
	}
	level := map[string]string{"probe": "stat", "stat": "stat", "list": "stat", "denied": "stat", "chdir": "stat",
		"write": "written", "create": "written", "delete": "deleted", "rename": "deleted"}[op]
	switch {
	case op == "read" && ev.Kind == "read":
		level = "read"
	case op == "read":
		level = "opened"
	}
	if level != "" {
		raiseDecoy(out, s.normalize(ev.Path), level)
	}
}

func raiseDecoy(out *FilesProbe, key, level string) {
	if slices.Index(decoyLevels, level) > slices.Index(decoyLevels, out.Decoys[key]) {
		out.Decoys[key] = level
	}
}
