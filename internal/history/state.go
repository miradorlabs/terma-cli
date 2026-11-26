// Package history discovers past local agent sessions, splits and routes them per
// working directory, skips sessions the destination team already holds, filters them
// through the relay content policy, and uploads them through a dedicated paced transport
// without touching the shared live relay outbox.
package history

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// Dir is the subdirectory under terma's state directory that holds per-route upload state.
const Dir = "history"

// ErrUploadRunning is returned when another `terma history upload` holds this profile's lock.
var ErrUploadRunning = errors.New("another `terma history upload` is already running for this profile")

const stateSchemaVersion = 1

// RouteState tracks per-route historical upload progress, isolated per sign-in account
// and profile so switching sign-in never mixes resume state.
type RouteState struct {
	Version    int                      `json:"version"`
	Account    string                   `json:"account"`
	Profile    string                   `json:"profile"`
	Enrollment string                   `json:"enrollment"`
	TeamID     string                   `json:"team_id"`
	InProgress map[string]SessionRecord `json:"in_progress,omitempty"`
	Done       map[string]SessionRecord `json:"done,omitempty"`
	Failed     map[string]FailedRecord  `json:"failed,omitempty"`
}

// SessionRecord summarizes one session in the in_progress or done map.
type SessionRecord struct {
	SourceSystem  string    `json:"source_system"`
	SessionID     string    `json:"session_id"`
	UpdatedAt     time.Time `json:"updated_at"`
	SourceModTime time.Time `json:"source_mod_time,omitempty"`
	Records       int       `json:"records,omitempty"`
	Bytes         int64     `json:"bytes,omitempty"`
}

// FailedRecord records a session permanently rejected by the ingest gateway.
type FailedRecord struct {
	SourceSystem    string    `json:"source_system"`
	SessionID       string    `json:"session_id"`
	FailedAt        time.Time `json:"failed_at"`
	Reason          string    `json:"reason"`
	RejectedRecords int64     `json:"rejected_records,omitempty"`
}

// SessionKey uniquely identifies a session across agent source systems.
func SessionKey(sourceSystem, sessionID string) string {
	return sourceSystem + ":" + sessionID
}

// NewRouteState returns an initialized empty state for one account/profile/enrollment route.
func NewRouteState(account, profile, enrollment, teamID string) RouteState {
	return RouteState{
		Version:    stateSchemaVersion,
		Account:    account,
		Profile:    profile,
		Enrollment: enrollment,
		TeamID:     teamID,
		InProgress: map[string]SessionRecord{},
		Done:       map[string]SessionRecord{},
		Failed:     map[string]FailedRecord{},
	}
}

// StatePath returns <stateDir>/history/@<account>/<profile>/<enrollment>.json after
// validating every path component so no caller can escape the state directory.
func StatePath(stateDir, account, profile, enrollment string) (string, error) {
	if stateDir == "" {
		return "", errors.New("history state requires a state directory")
	}
	for label, seg := range map[string]string{
		"account":    account,
		"profile":    profile,
		"enrollment": enrollment,
	} {
		if !validPathSegment(seg) {
			return "", fmt.Errorf("invalid history state %s %q", label, seg)
		}
	}
	return filepath.Join(stateDir, Dir, "@"+account, profile, enrollment+".json"), nil
}

// LockUpload acquires a non-blocking exclusive advisory lock for one account/profile upload
// so two concurrent runs never duplicate uploads or overwrite each other's RouteState.
func LockUpload(stateDir, account, profile string) (func(), error) {
	if stateDir == "" {
		return nil, errors.New("history lock requires a state directory")
	}
	for label, seg := range map[string]string{"account": account, "profile": profile} {
		if !validPathSegment(seg) {
			return nil, fmt.Errorf("invalid history lock %s %q", label, seg)
		}
	}
	lockPath := filepath.Join(stateDir, Dir, "@"+account, profile, "upload.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, err
	}
	unlock, err := flock.TryLock(lockPath)
	if err != nil {
		if flock.IsBusy(err) {
			return nil, ErrUploadRunning
		}
		return nil, err
	}
	return func() { flock.Remove(lockPath, unlock) }, nil
}

func validPathSegment(seg string) bool {
	return seg != "" &&
		seg == filepath.Base(seg) &&
		!strings.HasPrefix(seg, ".") &&
		!strings.ContainsAny(seg, `/\`)
}

// LoadState reads a route's resume state without creating or modifying files on disk.
// When the file does not exist it returns an initialized empty state and found=false.
func LoadState(stateDir, account, profile, enrollment, teamID string) (RouteState, bool, error) {
	path, err := StatePath(stateDir, account, profile, enrollment)
	if err != nil {
		return RouteState{}, false, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return NewRouteState(account, profile, enrollment, teamID), false, nil
	}
	if err != nil {
		return RouteState{}, false, err
	}
	var st RouteState
	if err := json.Unmarshal(b, &st); err != nil {
		return RouteState{}, false, fmt.Errorf("parse history state %s: %w", path, err)
	}
	if st.Account != account || st.Profile != profile || st.Enrollment != enrollment {
		return RouteState{}, false, fmt.Errorf("history state %s identity mismatch", path)
	}
	if st.InProgress == nil {
		st.InProgress = map[string]SessionRecord{}
	}
	if st.Done == nil {
		st.Done = map[string]SessionRecord{}
	}
	if st.Failed == nil {
		st.Failed = map[string]FailedRecord{}
	}
	if st.TeamID == "" {
		st.TeamID = teamID
	}
	return st, true, nil
}

// SaveState persists st atomically to its account- and route-isolated path.
func SaveState(stateDir string, st RouteState) error {
	path, err := StatePath(stateDir, st.Account, st.Profile, st.Enrollment)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	st.Version = stateSchemaVersion
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return config.WriteFileAtomic(path, b, 0o600)
}

// MarkInProgress records a session in in_progress before its first upload request.
// When the session is already in_progress with a recorded SourceModTime, the earlier
// snapshot timestamp is preserved so a resumed upload never re-sends newer live turns.
func (s *RouteState) MarkInProgress(sourceSystem, sessionID string, at time.Time, records int, bytes int64, sourceModTime ...time.Time) {
	if s.InProgress == nil {
		s.InProgress = map[string]SessionRecord{}
	}
	k := SessionKey(sourceSystem, sessionID)
	delete(s.Failed, k) // a failed session tried again (--retry-failed) is in progress now
	var mod time.Time
	if prev, ok := s.InProgress[k]; ok && !prev.SourceModTime.IsZero() {
		mod = prev.SourceModTime
	} else if len(sourceModTime) > 0 && !sourceModTime[0].IsZero() {
		mod = sourceModTime[0].UTC()
	}
	s.InProgress[k] = SessionRecord{
		SourceSystem:  sourceSystem,
		SessionID:     sessionID,
		UpdatedAt:     at.UTC(),
		SourceModTime: mod,
		Records:       records,
		Bytes:         bytes,
	}
}

// MarkDone moves a session from in_progress (or failed, when tried again) to done after all
// requests succeed with zero rejections.
func (s *RouteState) MarkDone(sourceSystem, sessionID string, at time.Time, records int, bytes int64) {
	if s.Done == nil {
		s.Done = map[string]SessionRecord{}
	}
	k := SessionKey(sourceSystem, sessionID)
	delete(s.InProgress, k)
	delete(s.Failed, k)
	s.Done[k] = SessionRecord{
		SourceSystem: sourceSystem,
		SessionID:    sessionID,
		UpdatedAt:    at.UTC(),
		Records:      records,
		Bytes:        bytes,
	}
}

// MarkFailed moves a session from in_progress to failed with the permanent rejection reason.
func (s *RouteState) MarkFailed(sourceSystem, sessionID string, at time.Time, reason string, rejected int64) {
	if s.Failed == nil {
		s.Failed = map[string]FailedRecord{}
	}
	k := SessionKey(sourceSystem, sessionID)
	delete(s.InProgress, k)
	s.Failed[k] = FailedRecord{
		SourceSystem:    sourceSystem,
		SessionID:       sessionID,
		FailedAt:        at.UTC(),
		Reason:          reason,
		RejectedRecords: rejected,
	}
}

// IsDone reports whether the session key has already completed for this route.
func (s *RouteState) IsDone(key string) bool {
	_, ok := s.Done[key]
	return ok
}

// IsFailed reports whether the session key previously failed permanently for this route.
func (s *RouteState) IsFailed(key string) (FailedRecord, bool) {
	rec, ok := s.Failed[key]
	return rec, ok
}

// IsInProgress reports whether the session key was interrupted mid-upload for this route.
func (s *RouteState) IsInProgress(key string) bool {
	_, ok := s.InProgress[key]
	return ok
}

func preloadStates(opts Options) (map[string]RouteState, map[string]map[string]bool, map[string]bool, error) {
	existing := map[string]RouteState{}
	byRoute := map[string]map[string]bool{}
	resumable := map[string]bool{}
	for _, t := range opts.State.Teams {
		st, _, err := LoadState(opts.StateDir, opts.Account, opts.Profile, t.EnrollmentID, t.TeamID)
		if err != nil {
			return nil, nil, nil, err
		}
		existing[t.EnrollmentID] = st
		retry := map[string]FailedRecord{}
		if opts.RetryFailed {
			retry = st.Failed
		}
		if len(st.InProgress)+len(retry) > 0 {
			m := make(map[string]bool, len(st.InProgress)+len(retry))
			for k := range st.InProgress {
				m[k], resumable[k] = true, true
			}
			for k := range retry {
				m[k], resumable[k] = true, true
			}
			byRoute[t.EnrollmentID] = m
		}
	}
	return existing, byRoute, resumable, nil
}

// appendAgedOutFailures keeps stored permanent failures visible in Summary.Failed even
// after the original transcript ages out of --since or is removed from disk.
func appendAgedOutFailures(teams []routing.Team, existing map[string]RouteState, labelFor func(routing.Team) string, sum *Summary) {
	seen := make(map[string]bool, len(sum.Failed))
	for _, f := range sum.Failed {
		seen[f.EnrollmentID+"\x00"+SessionKey(f.SourceSystem, f.SessionID)] = true
	}
	for _, t := range teams {
		st, ok := existing[t.EnrollmentID]
		if !ok || len(st.Failed) == 0 {
			continue
		}
		keys := make([]string, 0, len(st.Failed))
		for k := range st.Failed {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		label := labelFor(t)
		for _, k := range keys {
			if seen[t.EnrollmentID+"\x00"+k] {
				continue
			}
			frec := st.Failed[k]
			sum.Failed = append(sum.Failed, FailedSession{
				EnrollmentID:    t.EnrollmentID,
				TeamID:          t.TeamID,
				TeamLabel:       label,
				SourceSystem:    frec.SourceSystem,
				SessionID:       frec.SessionID,
				Reason:          frec.Reason,
				RejectedRecords: frec.RejectedRecords,
				Previously:      true,
				FailedAt:        frec.FailedAt,
			})
		}
	}
}
