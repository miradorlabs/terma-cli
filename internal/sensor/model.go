// Package sensor follows what an agent session does on the machine: the processes it
// starts, the bytes its connections and files move, the files it removes or renames, the
// containers it starts, and the connections and opens a policy denies it. A platform
// backend (eBPF on Linux; eslogger and pktap on macOS) reports events, and State keeps
// them per session for the local query API.
//
// A session is a root process: an agent's own process, which the sensor watches with
// every process it starts. Nothing outside a watched tree is kept.
package sensor

import (
	"context"
	"net/netip"
	"time"
)

// Proc is a process in a watched tree, or any exec while the sensor looks for roots.
type Proc struct {
	PID   int       `json:"pid"`
	PPID  int       `json:"ppid"`
	Root  int       `json:"root,omitempty"`
	Comm  string    `json:"comm,omitempty"`
	Exe   string    `json:"exe,omitempty"`
	Args  []string  `json:"args,omitempty"`
	Cgrp  uint64    `json:"cgroup,omitempty"`
	Start time.Time `json:"start"`
	Exit  time.Time `json:"exit,omitzero"`
}

// Flow is one socket's traffic: live while it is open, final once Closed.
type Flow struct {
	ID     uint64         `json:"id"`
	PID    int            `json:"pid"`
	Root   int            `json:"root"`
	Comm   string         `json:"comm,omitempty"`
	Proto  string         `json:"proto"`
	Local  netip.AddrPort `json:"local"`
	Remote netip.AddrPort `json:"remote"`
	Host   string         `json:"host,omitempty"`
	TX     uint64         `json:"tx"`
	RX     uint64         `json:"rx"`
	Start  time.Time      `json:"start"`
	Last   time.Time      `json:"last"`
	Closed bool           `json:"closed,omitempty"`
}

// File operations.
const (
	OpOpen   = "open"  // still open: Read and Written are what moved so far
	OpClose  = "close" // an opened file's last reference went; Read and Written say what moved
	OpUnlink = "unlink"
	OpRename = "rename"
)

// FileOp is a file a watched process used, removed or renamed.
type FileOp struct {
	Op      string    `json:"op"`
	PID     int       `json:"pid"`
	Root    int       `json:"root"`
	Comm    string    `json:"comm,omitempty"`
	Path    string    `json:"path"`
	To      string    `json:"to,omitempty"`
	Read    uint64    `json:"read,omitempty"`
	Written uint64    `json:"written,omitempty"`
	Flags   uint32    `json:"flags,omitempty"`
	Time    time.Time `json:"time"`
}

// Block is a connection or open the policy denied, or would have denied in audit mode.
type Block struct {
	Kind     string    `json:"kind"` // "connect" or "open"
	PID      int       `json:"pid"`
	Root     int       `json:"root"`
	Comm     string    `json:"comm,omitempty"`
	Rule     int       `json:"rule"`
	Enforced bool      `json:"enforced"`
	Target   string    `json:"target"`
	Time     time.Time `json:"time"`
}

// Sink receives a backend's events.
type Sink interface {
	Exec(Proc)
	Fork(Proc)
	Exit(pid, root int, at time.Time)
	FlowClosed(Flow)
	File(FileOp)
	Blocked(Block)
	// Name records that addr was reached as host, from a DNS answer or a TLS/QUIC hello.
	Name(addr netip.Addr, host string)
	// Hello records the host a connection's TLS/QUIC hello named.
	Hello(local, remote netip.AddrPort, host string)
}

// Backend is a platform's source of events and, where it can, its enforcement.
type Backend interface {
	Run(ctx context.Context, sink Sink) error
	// Roots makes every later exec of one of exes (by base name) a session's root, as it
	// happens.
	Roots(exes []string) error
	// Watch makes pid a session's root, and every process it starts part of the session.
	Watch(pid int) error
	// WatchCgroup counts every process in cgroup id (a container) toward root's session.
	WatchCgroup(id uint64, root int) error
	// Lend counts the container engine's processes toward root's session until Unlend:
	// the engine pulls and builds for a session's command while that command runs.
	Lend(root int) error
	Unlend(root int) error
	// Live is the current counters of the open flows.
	Live() []Flow
	// LiveFiles is the current counters of the open files; nil where the platform counts
	// a file's bytes only at close, or not at all.
	LiveFiles() []FileOp
	// Enforce applies a policy; a backend that cannot block returns ErrNoEnforcement.
	Enforce(p Resolved) error
}
