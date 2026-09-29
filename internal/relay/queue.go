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

	"github.com/miradorlabs/terma-cli/internal/config"
)

// The relay's queue is plain files, one request body each, so a crash at any point
// leaves either the file or its successor on disk and never a half of one:
//
//	inbox/<received>-<seq>-<signal>.<format>            accepted, not yet routed
//	outbox/<project>/<received>-<seq>-<signal>.<format>  routed, not yet delivered
//	dead/<project>-<received>-<seq>-<signal>.<format>    refused by the backend
//
// <received> is the Unix time in nanoseconds, zero-padded so a name sort is a time sort.
// Every file is written through config.WriteFileAtomicNoSync, whose temporary files
// start with ".tmp-" and are skipped by every listing.

const (
	inboxDir  = "inbox"
	outboxDir = "outbox"
	deadDir   = "dead"
	routesDir = "routes"

	formatJSON  = "json"
	formatProto = "pb"

	// machineRoute is the outbox of records that belong to the machine project. It is
	// resolved to a project id only when sent, so records routed before setup chose one
	// wait for it rather than being lost. It cannot collide with a project id: those are
	// letters, digits, dot, dash and underscore (project.ValidID).
	machineRoute = MachineRoute
)

// MachineRoute is how Health.HeldProjects names the machine project's records while no
// machine project is chosen.
const MachineRoute = "@machine"

// entry is one queued body, named by its file.
type entry struct {
	name     string
	received time.Time
	sig      signal
	format   string
}

var seq atomic.Uint64

func newEntry(now time.Time, sig signal, format string) entry {
	n := seq.Add(1) % 1_000_000
	name := fmt.Sprintf("%020d-%06d-%s.%s", now.UnixNano(), n, sig, format)
	return entry{name: name, received: now, sig: sig, format: format}
}

// parseEntry reads an entry back from its file name, false for anything else in the
// directory (a temporary file, a stray).
func parseEntry(name string) (entry, bool) {
	if strings.HasPrefix(name, ".") {
		return entry{}, false
	}
	stem, format, ok := strings.Cut(name, ".")
	if !ok || (format != formatJSON && format != formatProto) {
		return entry{}, false
	}
	parts := strings.SplitN(stem, "-", 3)
	if len(parts) != 3 {
		return entry{}, false
	}
	nanos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return entry{}, false
	}
	sig, ok := signalOfPath("/v1/" + parts[2])
	if !ok {
		return entry{}, false
	}
	return entry{name: name, received: time.Unix(0, nanos), sig: sig, format: format}, true
}

// listEntries is dir's queued bodies, oldest first; an absent directory is empty.
func listEntries(dir string) ([]entry, error) {
	des, err := os.ReadDir(dir)
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

// writeEntry stores body as e in dir, creating dir when needed.
func writeEntry(dir string, e entry, body []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return config.WriteFileAtomicNoSync(filepath.Join(dir, e.name), body, 0o600)
}

// validRoute admits an outbox directory name: a project id or the machine route.
func validRoute(route string) bool {
	if route == machineRoute {
		return true
	}
	if route == "" || len(route) > 128 || strings.HasPrefix(route, ".") {
		return false
	}
	for _, r := range route {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// routes lists the outbox's project directories.
func routes(outbox string) ([]string, error) {
	des, err := os.ReadDir(outbox)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, de := range des {
		if de.IsDir() && validRoute(de.Name()) {
			out = append(out, de.Name())
		}
	}
	return out, nil
}
