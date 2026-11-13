// Package reachtest is a migrated machine for an agent's reach test: what its hooks
// classify of the checkouts a call reached.
package reachtest

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Machine is a migrated machine for an agent's reach test: Acme Platform collects
// github.com/acme/*, bound to Acme, and personal only github.com/dawson/*, with a
// personal checkout, Side, and Acme's, API (named acme-api), beside it, each with a file
// (README.md, NOTES.md). Every folder outside git is nobody's. Unmigrated runs every hook
// as a machine not migrated does, with no routing.
type Machine struct {
	T          *testing.T
	StateDir   string
	Spool      *spool.Spool
	State      *routing.State
	Side, API  string
	Unmigrated bool
	// Stdout is what every hook printed.
	Stdout bytes.Buffer
}

// New is a Machine of its own.
func New(t *testing.T) *Machine {
	t.Helper()
	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	side := hookruntest.RepoAt(t, "github.com/dawson/side")
	api := filepath.Join(filepath.Dir(side), "acme-api")
	if err := os.Rename(hookruntest.RepoAt(t, "github.com/acme/api"), api); err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, side, "README.md", "side\n")
	hookruntest.WriteFile(t, api, "NOTES.md", "acme\n")
	st := hookruntest.RoutingState(t, []routing.Team{
		hookruntest.RoutedTeam("acme", "acme-platform", true, "github.com/acme/*"),
		hookruntest.RoutedPersonal("github.com/dawson/*"),
	}, map[string]string{"github.com/acme": "acme"})
	return &Machine{T: t, StateDir: t.TempDir(), Spool: sp, State: st, Side: side, API: api}
}

// Run is one hook of session sessionID of tool in cwd, as dispatch runs it: handler with
// payload on its stdin, then, when it claimed nothing, the session's placement from the
// payload.
func (m *Machine) Run(handler func(context.Context, hookrun.Env) error, sessionID, tool, cwd string, payload any) {
	m.T.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		m.T.Fatal(err)
	}
	claimed := false
	env := hookrun.Env{StateDir: m.StateDir, ConfigDir: m.T.TempDir(), Now: time.Now(), Cwd: cwd, Spool: m.Spool, Version: "test",
		Policy: config.NoPolicy("", ""), Stdin: bytes.NewReader(b), Stdout: &m.Stdout, OnClaim: func(string) { claimed = true }}
	if !m.Unmigrated {
		env.Routing = hookrun.NewRouting(hookruntest.RoutedProfile, m.State)
	}
	if err := handler(context.Background(), env); err != nil {
		m.T.Fatal(err)
	}
	if !claimed && !m.Unmigrated {
		hookrun.ClaimFromPayload(context.Background(), env, hookrun.PayloadSession{ID: sessionID, Cwd: cwd}, tool)
	}
}

// Mixed reports whether sessionID's run is Mixed, in the run ledger and its placement alike.
func (m *Machine) Mixed(sessionID string) bool {
	m.T.Helper()
	_, ledger := runs.MixedAt(m.StateDir, sessionID, time.Time{})
	c, ok := claim.Read(m.StateDir, sessionID, time.Now())
	if placement := ok && c.Kind == claim.KindMixed; placement != ledger {
		m.T.Fatalf("ledger Mixed %v, mixed placement %v", ledger, placement)
	}
	return ledger
}

// Locations are where the run ledger says sessionID's run has been.
func (m *Machine) Locations(sessionID string) []string {
	_, locs, _ := runs.Locations(m.StateDir, sessionID)
	return locs
}

// Wrote reports whether any hook wrote the run ledger or a placement: a machine not
// migrated writes neither.
func (m *Machine) Wrote() bool {
	for _, dir := range []string{filepath.Join(m.StateDir, runs.Dir), claim.Dir(m.StateDir)} {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			return true
		}
	}
	return false
}

// Outside is a folder outside git, which nobody collects.
func (m *Machine) Outside() string {
	d, _ := filepath.EvalSymlinks(m.T.TempDir())
	return d
}
