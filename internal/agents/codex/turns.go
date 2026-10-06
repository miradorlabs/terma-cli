package codex

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// turnAdmission judges a rollout turn by its own working directory, once per directory: a
// thread resumed in a listed repository still holds its earlier turns, which a hook there
// reads, and those must stay on the machine when the team does not collect where they ran.
func turnAdmission(ctx context.Context, e hookrun.Env) func(cwd string) bool {
	judged := map[string]bool{}
	return func(cwd string) bool {
		ok, seen := judged[cwd]
		if !seen {
			ok = cwd != "" && e.Admits(ctx, cwd)
			judged[cwd] = ok
		}
		return ok
	}
}

// turnFields are the payload fields that say where a thread runs.
type turnFields struct {
	Cwd            string `json:"cwd"`
	ThreadSettings struct {
		Cwd string `json:"cwd"`
	} `json:"thread_settings"`
}

// turnState is where the thread runs and whether the team collects its current turn.
type turnState struct {
	Cwd      string `json:"cwd,omitempty"`
	Admitted bool   `json:"admitted,omitempty"`
}

// track follows one record. Admission is judged whenever the thread moves (a resume
// announces its directory in thread_settings_applied before its turn), as each turn starts,
// and at its turn_context: review and compaction turns have none. It reports a turn the
// team does not collect.
func (s *turnState) track(admits func(string) bool, typ, payloadType string, f turnFields) (withheld bool) {
	switch {
	case typ == "session_meta":
		s.Cwd = f.Cwd
	case typ == "event_msg" && payloadType == "thread_settings_applied" && f.ThreadSettings.Cwd != "":
		s.Cwd = f.ThreadSettings.Cwd
		s.Admitted = admits(s.Cwd)
		return !s.Admitted
	case typ == "turn_context", typ == "event_msg" && (payloadType == "task_started" || payloadType == "turn_started"):
		if typ == "turn_context" && f.Cwd != "" {
			s.Cwd = f.Cwd
		}
		s.Admitted = admits(s.Cwd)
		return !s.Admitted
	}
	return false
}

// recheck judges a turn a previous read admitted again: the policy may have dropped its
// repository since. It reports a turn the team no longer collects.
func (s *turnState) recheck(admits func(string) bool) (withheld bool) {
	if s.Admitted {
		s.Admitted = admits(s.Cwd)
		return !s.Admitted
	}
	return false
}
