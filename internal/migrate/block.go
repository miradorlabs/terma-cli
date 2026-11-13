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
	// TwoInstalls: the agents' hook entries run another terma than the relay's
	// (Block.Others), which would go on with the rules the migration retires. Doctor names
	// the install to remove.
	TwoInstalls Reason = "two_installs"
	// Unchecked: the agents' hook entries could not be read, so which terma they run is
	// not known (Block.Detail).
	Unchecked Reason = "unchecked"
	// FlushBusy: a spool flush kept its lock for longer than the relay waits.
	FlushBusy Reason = "flush_busy"
	// OrgTypeUnknown: whether a profile's team is a personal organization's could not be
	// asked with that profile's sign-in (Block.Detail), and the migration needs it: a
	// personal team keeps collecting everything, any other's global policy collects nothing.
	// The machine keeps today's rules until a relay start can ask.
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
	// Unfinished: the machine is migrated (config.Migrated) but a routing state is still
	// Preparing, so nothing is collected until a relay start finishes the migration.
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

// block records b as why this start did not migrate. The record is advice for doctor:
// one that cannot be written changes nothing the migration does.
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

// MigratingFile is the state directory's record that a relay is migrating the machine now,
// and since when (Migrating). Run writes it before it asks or locks anything, and removes
// it once it has committed, recorded why not (BlockFile), or stopped with the relay; one
// a crash left stays until the next Run.
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

// migrating records that this Run is under way, and returns what removes the record. The
// record is advice for `terma teams` and doctor, as a block is.
func (m Machine) migrating() func() {
	path := filepath.Join(m.StateDir, MigratingFile)
	_ = os.MkdirAll(m.StateDir, 0o700)
	_ = config.WriteFileAtomic(path, []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n"), 0o600)
	return func() { _ = os.Remove(path) }
}
