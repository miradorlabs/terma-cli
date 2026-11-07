//go:build !linux && !darwin

// Package platform picks the sensor's backend for the operating system terma runs on.
package platform

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/sensor"
)

// DefaultSocket is where the query API would listen.
func DefaultSocket() string { return filepath.Join(os.TempDir(), "terma-sensor.sock") }

// Backend is absent here: Windows is designed (ETW to observe, WFP to block), not built.
type Backend struct {
	sensor.Backend
}

// New reports that this platform has no sensor yet.
func New(*slog.Logger) (*Backend, error) {
	return nil, errors.New("the sensor has no backend for this platform yet")
}

// Close does nothing.
func (b *Backend) Close() {}

// Running finds nothing.
func (b *Backend) Running([]string) []sensor.Proc { return nil }

// StartExtras starts nothing.
func (b *Backend) StartExtras(context.Context, *sensor.State, string, *slog.Logger) {}
