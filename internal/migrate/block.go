package migrate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// BlockFile is the state directory's record of why the last relay start did not migrate
// the machine, which doctor shows (ReadBlock). It goes once a start migrates.
const BlockFile = "migration-blocked.json"

// Reason is why a relay start did not migrate the machine.
type Reason string

// Reasons.
const (
	// TwoInstalls: the agents' hooks run another terma than the relay's (Block.Others),
	// which would keep the retired rules. Doctor names the install to remove.
	TwoInstalls Reason = "two_installs"
	// Unchecked: the agents' hook entries could not be read (Block.Detail).
	Unchecked Reason = "unchecked"
	// FlushBusy: a spool flush kept its lock for longer than the relay waits.
	FlushBusy Reason = "flush_busy"
	// OrgTypeUnknown: whether a profile's team is a personal organization's could not be
	// asked with its sign-in (Block.Detail). Today's rules stay until a start can ask.
	OrgTypeUnknown Reason = "org_type_unknown"
	// Failed: a step failed (Block.Detail). With Block.Unfinished the machine collects
	// nothing until a relay start finishes the migration.
	Failed Reason = "failed"
)

// Block is why the last relay start did not migrate the machine.
type Block struct {
	At     time.Time `json:"at"`
	Reason Reason    `json:"reason"`
	Detail string    `json:"detail,omitempty"`
	// Executable is the relay's own terma, and Others the termas the agents' hook entries
	// run instead.
	Executable string   `json:"executable,omitempty"`
	Others     []string `json:"others,omitempty"`
	// Unfinished: the machine is migrated but a routing state is still Preparing, so
	// nothing is collected until a relay start finishes.
	Unfinished bool `json:"unfinished,omitempty"`
}

// ReadBlock is why the last relay start under stateDir did not migrate the machine; false
// when it did, when none has tried, or when the record cannot be read.
func ReadBlock(stateDir string) (Block, bool) {
	b, err := os.ReadFile(filepath.Join(stateDir, BlockFile))
	if err != nil {
		return Block{}, false
	}
	var out Block
	if json.Unmarshal(b, &out) != nil || out.Reason == "" {
		return Block{}, false
	}
	return out, true
}

// block records b as why this start did not migrate: advice for doctor, best effort.
func (m Machine) block(b Block) {
	b.At = time.Now().UTC()
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(m.StateDir, 0o700)
	_ = config.WriteFileAtomic(filepath.Join(m.StateDir, BlockFile), append(data, '\n'), 0o600)
}

// clearBlock removes the record once the machine is migrated.
func clearBlock(stateDir string) { _ = os.Remove(filepath.Join(stateDir, BlockFile)) }

// MigratingFile records under the state directory since when a relay has been migrating
// (Migrating). Run writes it first and removes it at the commit, a block, or a stop; one a
// crash left stays until the next Run.
const MigratingFile = "migrating"

// Migrating reports since when a relay under stateDir has been migrating the machine;
// false when none is.
func Migrating(stateDir string) (time.Time, bool) {
	b, err := os.ReadFile(filepath.Join(stateDir, MigratingFile))
	if err != nil {
		return time.Time{}, false
	}
	since, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b)))
	return since, err == nil
}

// migrating records that this Run is under way, for `terma teams` and doctor, and returns
// what removes the record.
func (m Machine) migrating() func() {
	path := filepath.Join(m.StateDir, MigratingFile)
	_ = os.MkdirAll(m.StateDir, 0o700)
	_ = config.WriteFileAtomic(path, []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n"), 0o600)
	return func() { _ = os.Remove(path) }
}
