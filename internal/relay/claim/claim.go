// Package claim is how hooks tell the local relay which sessions belong to an opted-in
// repository. A hook in a repository with a project binding writes one small file per
// session; the relay forwards an agent's telemetry only for sessions it finds here.
//
// It is its own package, with no OTLP dependency, because every hook imports it and
// the hook path has a latency budget; the relay is the only reader.
package claim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// DirName is the relay's directory under the config dir. It holds the claims, the
// local token the agents' exporters present, the address the relay listens on, its
// single-instance lock and the stats it leaves behind.
const DirName = "relay"

const (
	claimsDir = "claims"
	tokenFile = "token"
)

// TTL is how long a claim stays valid after its last write. A hook refreshes it on
// every event of a live session (see Refresh), so a session idle this long has ended;
// it matches the hooks' ActiveTTL.
const TTL = 4 * time.Hour

// Refresh is how old a claim may be before a hook rewrites it. Below it a hook only
// stats the file, so a session's hundreds of tool calls cost one write every few
// minutes, not one each.
const Refresh = 5 * time.Minute

// Claim says which project a session's telemetry belongs to.
type Claim struct {
	ProjectID string    `json:"project_id"`
	Tool      string    `json:"tool,omitempty"`
	Repo      string    `json:"repo,omitempty"`
	Worktree  string    `json:"worktree,omitempty"`
	ClaimedAt time.Time `json:"claimed_at"`
	// PIDs are the processes the claiming hooks ran under: the agent is one of them.
	// The relay forwards a record only from a process named here, so the same session
	// resumed by another process elsewhere — where no hook of this repository runs —
	// is not covered. Every hook of the session adds its own; the most recent maxPIDs
	// are kept. Empty (a platform where they cannot be read) matches any sender.
	PIDs []int `json:"pids,omitempty"`
}

// maxPIDs bounds a claim's process list: a few runs of one session, each with its
// chain of ancestors.
const maxPIDs = 64

// Covers reports whether pid may send under this claim.
func (c Claim) Covers(pid int) bool {
	if len(c.PIDs) == 0 || pid == 0 {
		return true
	}
	return slices.Contains(c.PIDs, pid)
}

// Dir is the relay directory, under the config dir.
func Dir() (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, DirName), nil
}

// Enabled reports whether this machine exports through the relay: `terma relay setup`
// wrote its token. Without it hooks write no claims — nothing would read them.
func Enabled() bool {
	dir, err := Dir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(dir, tokenFile))
	return err == nil
}

// TokenPath is where the relay's local token lives.
func TokenPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, tokenFile), nil
}

func path(sessionID string) (string, bool) {
	if !session.ValidID(sessionID) {
		return "", false
	}
	dir, err := Dir()
	if err != nil {
		return "", false
	}
	return filepath.Join(dir, claimsDir, sessionID+".json"), true
}

// Write records that sessionID belongs to c.ProjectID, unless a claim for the same
// project was written less than Refresh ago. It reports whether it wrote. Errors are
// swallowed: a hook never fails for want of a claim, the session is only not exported.
func Write(sessionID string, c Claim, now time.Time) bool {
	if c.ProjectID == "" {
		return false
	}
	p, ok := path(sessionID)
	if !ok {
		return false
	}
	prev, havePrev := read(p)
	if havePrev && prev.ProjectID == c.ProjectID {
		if info, err := os.Stat(p); err == nil && now.Sub(info.ModTime()) < Refresh && subset(c.PIDs, prev.PIDs) {
			return false
		}
		c.PIDs = merge(prev.PIDs, c.PIDs)
	}
	c.ClaimedAt = now.UTC()
	data, err := json.Marshal(c)
	if err != nil {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return false
	}
	return config.WriteFileAtomicNoSync(p, data, 0o600) == nil
}

func subset(a, b []int) bool {
	for _, v := range a {
		if !slices.Contains(b, v) {
			return false
		}
	}
	return true
}

// merge appends the new pids to the old ones, without repeats, keeping the newest.
func merge(old, add []int) []int {
	out := slices.Clone(old)
	for _, v := range add {
		if i := slices.Index(out, v); i >= 0 {
			out = slices.Delete(out, i, i+1)
		}
		out = append(out, v)
	}
	if len(out) > maxPIDs {
		out = out[len(out)-maxPIDs:]
	}
	return out
}

// Read returns the live claim for sessionID: one written less than TTL ago.
func Read(sessionID string, now time.Time) (Claim, bool) {
	p, ok := path(sessionID)
	if !ok {
		return Claim{}, false
	}
	info, err := os.Stat(p)
	if err != nil || now.Sub(info.ModTime()) >= TTL {
		return Claim{}, false
	}
	return read(p)
}

func read(p string) (Claim, bool) {
	data, err := os.ReadFile(p)
	if err != nil {
		return Claim{}, false
	}
	var c Claim
	if json.Unmarshal(data, &c) != nil || c.ProjectID == "" {
		return Claim{}, false
	}
	return c, true
}

// Prune removes claims older than TTL. The relay runs it; hooks never do.
func Prune(now time.Time) {
	dir, err := Dir()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(filepath.Join(dir, claimsDir))
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err == nil && now.Sub(info.ModTime()) >= TTL {
			_ = os.Remove(filepath.Join(dir, claimsDir, e.Name()))
		}
	}
}
