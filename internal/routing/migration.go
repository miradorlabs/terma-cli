package routing

import (
	"errors"
	"fmt"
)

// MigrationPhase is how far the machine's move to these routing rules has got, as one
// profile's routing state records it.
type MigrationPhase string

// Migration phases. Until a state is Committed, hooks and delivery run the earlier
// single-team rules exactly; a crash in between leaves a machine that collects nothing
// until the next relay start redoes the recorded steps and commits.
const (
	// Preparing: the state is written with its stable enrollments, and the keys and
	// sign-ins are being copied to the v2 indexes and retired from the legacy ones.
	Preparing MigrationPhase = "preparing"
	// Committed: the legacy entries are gone and these rules apply.
	Committed MigrationPhase = "committed"
)

// Migration is the record of how a profile's routing state was made from its single team.
type Migration struct {
	Phase MigrationPhase `json:"phase"`
	// Legacy maps a legacy project ID, as records queued before the migration name it, to
	// its cutover enrollment. It is the only way such a record reaches an enrollment: one
	// naming any other project is dropped, and so is one whose enrollment has ended.
	Legacy map[string]string `json:"legacy,omitempty"`
}

// check reports the first way m breaks its shape against s's teams.
func (m *Migration) check(s State) error {
	if m == nil {
		return nil
	}
	if m.Phase != Preparing && m.Phase != Committed {
		return fmt.Errorf("unknown migration phase %q", m.Phase)
	}
	for project, enrollment := range m.Legacy {
		switch {
		case project == "":
			// A record naming no project was never collected: it maps to nothing.
			return errors.New("the migration maps an empty legacy project")
		case !s.Selected(enrollment):
			return fmt.Errorf("the migration maps legacy project %s to enrollment %q, which is not selected", project, enrollment)
		}
	}
	return nil
}

// forgetLegacy removes every legacy mapping to enrollmentID, which has ended: its legacy
// records are dropped with it, even if the team is ticked again later.
func (m *Migration) forgetLegacy(enrollmentID string) {
	if m == nil {
		return
	}
	for project, enrollment := range m.Legacy {
		if enrollment == enrollmentID {
			delete(m.Legacy, project)
		}
	}
}
