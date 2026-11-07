//go:build linux

// Package platform picks the sensor's backend for the operating system terma runs on.
package platform

import (
	"context"
	"log/slog"

	"github.com/miradorlabs/terma-cli/internal/sensor"
	"github.com/miradorlabs/terma-cli/internal/sensor/linux"
)

// DefaultSocket is where the query API listens: the sensor runs as root on Linux.
func DefaultSocket() string { return "/run/terma-sensor.sock" }

// Backend is the platform's backend with what runs beside it.
type Backend struct {
	sensor.Backend
	ebpf *linux.Backend
}

// New loads the eBPF programs.
func New(*slog.Logger) (*Backend, error) {
	b, err := linux.New()
	if err != nil {
		return nil, err
	}
	return &Backend{Backend: b, ebpf: b}, nil
}

// Close detaches the programs.
func (b *Backend) Close() { b.ebpf.Close() }

// Running lists the processes already running under one of the root names.
func (b *Backend) Running(names []string) []sensor.Proc { return linux.Running(names) }

// StartExtras starts the names capture and the container watcher.
func (b *Backend) StartExtras(ctx context.Context, st *sensor.State, docker string, log *slog.Logger) {
	go func() {
		if err := b.ebpf.CaptureNames(ctx, st); err != nil {
			log.Warn("names capture", "err", err)
		}
	}()
	if docker != "" {
		st.LendEngine()
		go sensor.WatchDocker(ctx, st, docker, linux.CgroupOf, log)
	}
}
