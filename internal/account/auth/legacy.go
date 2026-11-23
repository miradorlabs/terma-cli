package auth

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// A terma before user tokens signed in once per organization, and kept each sign-in in
// credentials.json, its tokens in the keychain item credential/<profile>/<organization>.
// Those sessions are retired: signing in deletes them, after a best-effort revoke.

// LegacySignIn is one sign-in an older terma left, as credentials.json indexes it.
type LegacySignIn struct {
	Profile        string
	OrganizationID string
	SessionID      string
	UserID         string
	AuthURL        string
	// AccessToken is "" when its keychain item is gone or could not be read.
	AccessToken string
	// inFile is that credentials.json holds the tokens, and no keychain item may: an
	// older terma with no keychain kept them there.
	inFile bool
}

type legacyCredential struct {
	AccessToken       string `json:"access_token,omitempty"`
	StaleKeychainItem bool   `json:"stale_keychain_item,omitempty"`
	SessionID         string `json:"session_id,omitempty"`
	AuthURL           string `json:"auth_url,omitempty"`
	OrganizationID    string `json:"organization_id,omitempty"`
	UserID            string `json:"user_id,omitempty"`
}

type legacyFile map[string]*struct {
	Organizations map[string]*legacyCredential `json:"organizations"`
}

func legacyItemName(profile, organizationID string) string {
	return "credential/" + profile + "/" + organizationID
}

// LegacySignIns lists the sign-ins an older terma left under the config directory dir,
// ordered by profile and organization, none when there is no file. With tokens, it reads
// each one's access token from the keychain when the file does not hold it, for a revoke;
// a keychain that cannot give one up leaves it "".
func LegacySignIns(dir string, tokens bool) ([]LegacySignIn, error) {
	signIns, _, err := ReadLegacySignIns(dir, tokens)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return signIns, err
}

// ReadLegacySignIns is LegacySignIns from one read of the file, which also says whether it
// parsed whole: a file that doesn't (one profile readable beside a broken one) names no one
// its backlog may be taken for. A file that can't be read, or isn't there, is an error, so
// no one is decided from it.
func ReadLegacySignIns(dir string, tokens bool) (signIns []LegacySignIn, whole bool, err error) {
	data, err := os.ReadFile(config.LegacyCredentialsPath(dir))
	if err != nil {
		return nil, false, fmt.Errorf("read the older sign-ins: %w", err)
	}
	var file legacyFile
	// A file that doesn't parse is still one to delete, and what can be read of it is still
	// revoked and forgotten.
	whole = json.Unmarshal(data, &file) == nil
	var out []LegacySignIn
	for profile, p := range file {
		if p == nil {
			continue
		}
		for org, c := range p.Organizations {
			if c == nil {
				continue
			}
			s := LegacySignIn{Profile: profile, OrganizationID: org, SessionID: c.SessionID, UserID: c.UserID,
				AuthURL: c.AuthURL, AccessToken: c.AccessToken, inFile: c.AccessToken != "" && !c.StaleKeychainItem}
			if tokens && s.AccessToken == "" {
				if v, err := secret.Get(dir, legacyItemName(profile, org)); err == nil {
					var pair tokenPair
					if json.Unmarshal([]byte(v), &pair) == nil {
						s.AccessToken = pair.AccessToken
					}
				}
			}
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b LegacySignIn) int {
		return cmp.Or(cmp.Compare(a.Profile, b.Profile), cmp.Compare(a.OrganizationID, b.OrganizationID))
	})
	return out, whole, nil
}

// HasLegacySignIns reports whether an older terma left sign-ins under dir.
func HasLegacySignIns(dir string) bool {
	_, err := os.Stat(config.LegacyCredentialsPath(dir))
	return err == nil
}

// ForgetLegacy deletes the sign-ins an older terma left: every keychain item
// credentials.json names, then the file (its lock goes with WithLegacySignIns). A keychain
// item that cannot be removed is reported, and the file stays so a later sign-in tries
// again. A sign-in the file holds the tokens of has no item to remove, as forget judges it,
// so a machine with no keychain loses its older sign-ins too.
func ForgetLegacy(dir string) error {
	signIns, err := LegacySignIns(dir, false)
	if err != nil {
		return err
	}
	var errs []error
	for _, s := range signIns {
		if s.inFile {
			continue
		}
		if err := secret.Delete(dir, legacyItemName(s.Profile, s.OrganizationID)); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("could not remove %d older sign-ins from the system keychain: %w", len(errs), errors.Join(errs...))
	}
	path := config.LegacyCredentialsPath(dir)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// WithLegacySignIns runs fn holding the lock an older terma writes credentials.json
// under, so the sign-ins a retirement reads, records and forgets are one snapshot: no
// older terma signing in meanwhile can replace the file between. It waits up to
// legacyLockWait. Once fn has forgotten the file, its lock goes too.
func WithLegacySignIns(ctx context.Context, dir string, fn func()) error {
	ctx, cancel := context.WithTimeout(ctx, legacyLockWait)
	defer cancel()
	path := config.LegacyCredentialsPath(dir)
	unlock, err := lockCredentialFile(ctx, path)
	if err != nil {
		return err
	}
	fn()
	// Decided while held: removed after its unlock, a waiter could take the lock and lose
	// the file to a third process's new one, and two would hold it.
	if HasLegacySignIns(dir) {
		unlock()
	} else {
		flock.Remove(path+".lock", unlock)
	}
	return nil
}

// legacyLockWait bounds the wait for an older terma that holds credentials.json's lock, as
// one does through a refresh.
var legacyLockWait = 10 * time.Second

// LegacyRecordFile is, in the state directory, what the first sign-in on user tokens kept of
// the sign-ins it retired.
const LegacyRecordFile = "legacy-sign-ins.json"

// LegacyReportWindow is how long the heartbeat reports the retired sessions.
const LegacyReportWindow = 30 * 24 * time.Hour

// LegacyRecord is what a retirement kept of the per-organization sign-ins it retired: their
// session ids, which the heartbeat reports for LegacyReportWindow so the platform matches
// this installation to the machine they stood for, and whose what an older terma queued
// is. Account is that one decision, taken once, before the sign-ins are deleted: a
// person's account key, LegacyNoPerson, or LegacyAccountAmbiguous. "" is no decision yet,
// and nothing an older terma queued is taken in until there is one. RetiredAt is when it
// was taken, for the heartbeat's window only.
type LegacyRecord struct {
	SessionIDs []string  `json:"session_ids,omitempty"`
	Account    string    `json:"account,omitempty"`
	RetiredAt  time.Time `json:"retired_at"`
}

// The older queues the decision is taken into once: the spool, with what an older terma
// parked or recorded as it goes there, and the relay's outbox.
const (
	LegacySpool  = "spool"
	LegacyOutbox = "outbox"
)

// LegacyStage sets aside what an older terma holds in its queue Queue (LegacySpool or
// LegacyOutbox) as the decision of whose it is is taken (RecordLegacy), to be taken in under
// it: what the older terma queues after goes to a queue of its own, which is ambiguous. A
// stage that finds something set aside already, by a run that stopped before recording the
// decision, does nothing.
type LegacyStage struct {
	Queue string
	Stage func() error
}

// LegacyAccountAmbiguous is the decision for retired sign-ins that named more than one
// person or environment, or none, or a file that did not read whole: what an older terma
// queued was collected by people, but no one sign-in may send it, and neither may a server
// key. It ages out.
const LegacyAccountAmbiguous = "ambiguous"

// LegacyNoPerson is the decision on a server-key machine: what an older terma queued there
// leaves only under the server key.
const LegacyNoPerson = sendas.NoPersonLane

// LegacyLane is the lane what an older terma queued goes to under decision, the record's
// Account: a person's account key, LegacyNoPerson, or LegacyAccountAmbiguous for anything
// else, which never leaves. "" is no decision yet. The spool and the relay both take it.
func LegacyLane(decision string) string {
	if decision == "" || decision == LegacyNoPerson || ValidAccountKey(decision) {
		return decision
	}
	return LegacyAccountAmbiguous
}

// Recent reports whether the record's sessions are still reported at now.
func (r LegacyRecord) Recent(now time.Time) bool {
	return !r.RetiredAt.IsZero() && now.Sub(r.RetiredAt) < LegacyReportWindow
}

// LegacyOwner decides whose what an older terma queued is, from its sign-ins: on a
// server-key machine (serverKey), no person's; else the one person they all name, when
// the file they came from read whole (whole); else ambiguous. Nothing missing or unread
// is ever taken for a server key's: only serverKey says so.
func LegacyOwner(signIns []LegacySignIn, whole, serverKey bool) string {
	if serverKey {
		return LegacyNoPerson
	}
	owner := ""
	for _, s := range signIns {
		a := AccountKey(s.AuthURL, s.UserID)
		if a == "" || owner != "" && a != owner {
			return LegacyAccountAmbiguous
		}
		owner = a
	}
	if !whole || owner == "" {
		return LegacyAccountAmbiguous
	}
	return owner
}

// RecordLegacy keeps, under the state directory stateDir, owner as the decision for what an
// older terma queued, and signIns' sessions for the heartbeat. The first decision written
// stands: a later one only adds session ids. A record that can't be read is not taken for
// none, and nothing is written. The decision is taken only once every one of stages has set
// its older queue aside, under the same lock, so nothing an older terma queues after it is
// ever taken in under it: with one that fails, nothing is written, and the error says so.
func RecordLegacy(stateDir string, signIns []LegacySignIn, owner string, now time.Time, stages ...LegacyStage) error {
	return withLegacyRecord(stateDir, func() error {
		rec, err := ReadLegacyRecord(stateDir)
		if err != nil {
			return err
		}
		changed := rec.Account == ""
		if changed {
			for _, s := range stages {
				if err := s.Stage(); err != nil {
					return fmt.Errorf("set aside an older terma's %s: %w", s.Queue, err)
				}
			}
			rec.Account, rec.RetiredAt = owner, now.UTC()
		}
		for _, s := range signIns {
			if s.SessionID != "" && !slices.Contains(rec.SessionIDs, s.SessionID) {
				rec.SessionIDs = append(rec.SessionIDs, s.SessionID)
				changed = true
			}
		}
		if !changed {
			return nil // a record kept as it was is not written again
		}
		return config.WriteJSON(filepath.Join(stateDir, LegacyRecordFile), rec, 0o600)
	})
}

// LegacyOwnership is the record of whose an older terma's backlog is. On a profile set up
// with a server key (serverKey), it first records the server key's decision, no person's,
// unless one is kept already: the relay looks as it starts, and delivery as it flushes, and
// what an older terma queued on a server-key machine is the key's, whether or not it left
// sign-ins. On a person's profile nothing is recorded: the decision waits for the
// retirement at their sign-in.
func LegacyOwnership(dir, stateDir string, serverKey bool, stages ...LegacyStage) (LegacyRecord, error) {
	if serverKey {
		// Sessions that can't be read are none for the heartbeat; the decision is the key's
		// all the same.
		signIns, _ := LegacySignIns(dir, false)
		if err := RecordLegacy(stateDir, signIns, LegacyNoPerson, time.Now(), stages...); err != nil {
			return LegacyRecord{}, err
		}
	}
	return ReadLegacyRecord(stateDir)
}

// withLegacyRecord runs fn holding the legacy record's lock: every write reads the record
// first and keeps one already kept, which only holds if no other write lands in between.
func withLegacyRecord(stateDir string, fn func() error) error {
	return flock.Locked(filepath.Join(stateDir, LegacyRecordFile), legacyRecordWait, fn)
}

// legacyRecordWait bounds the wait for the legacy record's lock; its holders only read and
// write one small file.
const legacyRecordWait = 5 * time.Second

// ReadLegacyRecord is what RecordLegacy kept, the zero record when nothing was.
func ReadLegacyRecord(stateDir string) (LegacyRecord, error) {
	var rec LegacyRecord
	data, err := os.ReadFile(filepath.Join(stateDir, LegacyRecordFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return rec, nil
		}
		return rec, err
	}
	return rec, json.Unmarshal(data, &rec)
}
