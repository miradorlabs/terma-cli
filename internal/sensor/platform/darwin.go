//go:build darwin

// Package platform picks the sensor's backend for the operating system terma runs on.
package platform

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/miradorlabs/terma-cli/internal/sensor"
	"github.com/miradorlabs/terma-cli/internal/sensor/darwin"
)

// DefaultSocket is where the query API listens.
func DefaultSocket() string { return filepath.Join(os.TempDir(), "terma-sensor.sock") }

// Backend is the platform's backend with what runs beside it.
type Backend struct {
	sensor.Backend
}

// New serves TermaSensor.app's system extensions.
func New(log *slog.Logger) (*Backend, error) {
	return &Backend{Backend: darwin.New(log)}, nil
}

// Close does nothing: the extensions outlive the sensor and reconnect to the next one.
func (b *Backend) Close() {}

// Running lists the processes already running under one of the root names; the kernel
// keeps a process's name cut to 16 bytes.
func (b *Backend) Running(names []string) []sensor.Proc {
	want := map[string]bool{}
	for _, e := range names {
		want[e[:min(len(e), 16)]] = true
	}
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil
	}
	comm := map[int]string{}
	for _, p := range procs {
		comm[int(p.Proc.P_pid)] = unix.ByteSliceToString(p.Proc.P_comm[:])
	}
	var out []sensor.Proc
	for _, p := range procs {
		pid, ppid := int(p.Proc.P_pid), int(p.Eproc.Ppid)
		if want[comm[pid]] && !want[comm[ppid]] {
			out = append(out, sensor.Proc{PID: pid, PPID: ppid, Exe: comm[pid]})
		}
	}
	return out
}

// StartExtras starts the Docker watcher: the extensions bring the names, and containers run
// in the engine's VM, attributed through the engine's API as on Linux, minus the cgroup.
func (b *Backend) StartExtras(ctx context.Context, st *sensor.State, docker string, log *slog.Logger) {
	if docker != "" {
		go sensor.WatchDocker(ctx, st, docker, nil, log)
	}
}
