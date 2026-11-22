package auth

import (
	"cmp"
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
// ordered by profile and organization. With tokens, it reads each one's access token from
// the keychain when the file does not hold it, for a revoke; a keychain that cannot give
// one up leaves it "".
func LegacySignIns(dir string, tokens bool) ([]LegacySignIn, error) {
	data, err := os.ReadFile(config.LegacyCredentialsPath(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the older sign-ins: %w", err)
	}
	var file legacyFile
	// An unreadable file is still one to delete, and what can be read of it is still
	// revoked and forgotten; whose its backlog is, LegacyParsedWhole says.
	_ = json.Unmarshal(data, &file)
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
	return out, nil
}

// LegacyParsedWhole reports whether the sign-ins an older terma left under dir read whole:
// a file that doesn't (one profile readable beside a broken one) names no one its backlog
// may be taken for.
func LegacyParsedWhole(dir string) bool {
	data, err := os.ReadFile(config.LegacyCredentialsPath(dir))
	var file legacyFile
	return err == nil && json.Unmarshal(data, &file) == nil
}

// HasLegacySignIns reports whether an older terma left sign-ins under dir.
func HasLegacySignIns(dir string) bool {
	_, err := os.Stat(config.LegacyCredentialsPath(dir))
	return err == nil
}

// ForgetLegacy deletes the sign-ins an older terma left: every keychain item
// credentials.json names, then the file and its lock. A keychain item that cannot be
// removed is reported, and the file stays so a later sign-in tries again. A sign-in the
// file holds the tokens of has no item to remove, as forget judges it, so a machine with
// no keychain loses its older sign-ins too.
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
	_ = os.Remove(path + ".lock")
	return nil
}

// LegacyRecordFile is, in the state directory, what the first sign-in on user tokens kept of
// the sign-ins it retired.
const LegacyRecordFile = "legacy-sign-ins.json"

// LegacyReportWindow is how long the heartbeat reports the retired sessions.
const LegacyReportWindow = 30 * 24 * time.Hour

// LegacyRecord is what a sign-in kept of the per-organization sign-ins it retired: their
// session ids, which the heartbeat reports for LegacyReportWindow so the platform matches
// this installation to the machine they stood for, and the account they all belonged to,
// whose name what an older terma queued may still leave under. Account is
// LegacyAccountAmbiguous when they named no single person and environment.
type LegacyRecord struct {
	SessionIDs []string  `json:"session_ids,omitempty"`
	Account    string    `json:"account,omitempty"`
	RetiredAt  time.Time `json:"retired_at"`
	// ForgottenAt is when a retirement last set out to forget the older sign-ins, and when
	// one did, its last: until then an older terma's hook may still have queued, so what
	// has no account from before then is theirs in the spool (ReplayBefore). RetiredAt
	// stays the relay's boundary, which a server key's parts are judged by.
	ForgottenAt time.Time `json:"forgotten_at,omitzero"`
}

// ReplayBefore is the boundary of what an older terma's hooks queued: an event bound to no
// account from before it is the older sign-ins'. The later of RetiredAt and ForgottenAt,
// so a retirement that took more than one try still holds what was queued between them.
func (r LegacyRecord) ReplayBefore() time.Time {
	if r.ForgottenAt.After(r.RetiredAt) {
		return r.ForgottenAt
	}
	return r.RetiredAt
}

// LegacyAccountAmbiguous is the account of retired sign-ins that named more than one
// person or environment, or none: what an older terma queued then was collected by
// people, but no one sign-in may send it, and neither may a server key. It ages out.
const LegacyAccountAmbiguous = "ambiguous"

// Recent reports whether the record's sessions are still reported at now.
func (r LegacyRecord) Recent(now time.Time) bool {
	return !r.RetiredAt.IsZero() && now.Sub(r.RetiredAt) < LegacyReportWindow
}

// RecordLegacy keeps, under the state directory stateDir, what signIns were: the first
// retirement's record stands, so a later one only adds session ids.
func RecordLegacy(stateDir string, signIns []LegacySignIn, now time.Time) error {
	if len(signIns) == 0 {
		return nil
	}
	return withLegacyRecord(stateDir, func() error { return recordLegacy(stateDir, signIns, now) })
}

func recordLegacy(stateDir string, signIns []LegacySignIn, now time.Time) error {
	if len(signIns) == 0 {
		return nil
	}
	// A record that can't be read is not taken for none: this retirement is not the first,
	// and the account it kept must not be replaced.
	rec, err := ReadLegacyRecord(stateDir)
	if err != nil {
		return err
	}
	fresh := rec.RetiredAt.IsZero()
	if fresh {
		rec.RetiredAt = now.UTC()
	}
	accounts := map[string]bool{}
	for _, s := range signIns {
		if s.SessionID != "" && !slices.Contains(rec.SessionIDs, s.SessionID) {
			rec.SessionIDs = append(rec.SessionIDs, s.SessionID)
		}
		accounts[AccountKey(s.AuthURL, s.UserID)] = true
	}
	if fresh {
		rec.Account = LegacyAccountAmbiguous
		if len(accounts) == 1 {
			for a := range accounts {
				rec.Account = cmp.Or(a, LegacyAccountAmbiguous)
			}
		}
	}
	return config.WriteJSON(filepath.Join(stateDir, LegacyRecordFile), rec, 0o600)
}

// LegacyStillSignedIn is a boundary nothing is after: while an older terma's sign-ins are
// still on the machine, its hooks may still be queueing, so every event bound to no
// account is theirs.
var LegacyStillSignedIn = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// UnreadableLegacyRecord stands in for stateDir's record when it exists but can't be
// read: an entry bound to no account counts as named by no one if it arrived before the
// record was last written, which is no earlier than the retirement. So a server key sends
// none of an older terma's backlog, and still sends what it collects after. With no time
// for the record either, every such entry counts as named by no one.
func UnreadableLegacyRecord(stateDir string) LegacyRecord {
	rec := LegacyRecord{Account: LegacyAccountAmbiguous, RetiredAt: LegacyStillSignedIn}
	if info, err := os.Stat(filepath.Join(stateDir, LegacyRecordFile)); err == nil {
		rec.RetiredAt = info.ModTime().UTC()
	}
	return rec
}

// LegacyOwnership is whose an older terma's backlog is, for the relay and delivery to
// judge: the record when one was kept. With older sign-ins still on the machine and no
// record, a profile set up with a server key (serverKey) records one now, the first time
// this terma looks (the relay looks as it starts, before any export): everything bound to
// no account until now is named by no one, for good, so the key sends only what it
// collects after, and no later sign-in sends what the key collected. On a person's
// profile nothing is recorded: what is bound to no account waits for the retirement at
// their sign-in. A record that can't be written leaves every such entry, up to now, named
// by no one. A record that can't be read is as UnreadableLegacyRecord.
func LegacyOwnership(dir, stateDir string, serverKey bool) (LegacyRecord, error) {
	return LegacyOwnershipAt(dir, stateDir, serverKey, time.Now())
}

// LegacyOwnershipAt is LegacyOwnership with at as the first look's boundary: a caller whose
// first look could not be written tries again with the same one, so the boundary it has
// been judging by is the one recorded.
func LegacyOwnershipAt(dir, stateDir string, serverKey bool, at time.Time) (LegacyRecord, error) {
	rec, err := ReadLegacyRecord(stateDir)
	if err != nil || !rec.RetiredAt.IsZero() || !serverKey || !HasLegacySignIns(dir) {
		return rec, err
	}
	nameless := LegacyRecord{Account: LegacyAccountAmbiguous, RetiredAt: at.UTC()}
	if signIns, err := LegacySignIns(dir, false); err == nil {
		for _, s := range signIns {
			if s.SessionID != "" {
				nameless.SessionIDs = append(nameless.SessionIDs, s.SessionID)
			}
		}
	}
	// Written only while there is still no record, under the record's lock: a retirement
	// writing the older sign-ins' own account at the same moment is never overwritten.
	write := func() error {
		if rec, err := ReadLegacyRecord(stateDir); err != nil || !rec.RetiredAt.IsZero() {
			return err
		}
		return config.WriteJSON(filepath.Join(stateDir, LegacyRecordFile), nameless, 0o600)
	}
	if withLegacyRecord(stateDir, write) != nil {
		return nameless, nil
	}
	if written, err := ReadLegacyRecord(stateDir); err == nil && !written.RetiredAt.IsZero() {
		return written, nil
	}
	return nameless, nil
}

// AdvanceLegacyBoundary moves the record's boundary from from to to, only while it is still
// from: the retirement that wrote it moves it to when its work is done, so what an older
// terma's hook still running queued meanwhile stays on that terma's side. A record any
// earlier look or retirement wrote keeps its own boundary.
func AdvanceLegacyBoundary(stateDir string, from, to time.Time) error {
	return withLegacyRecord(stateDir, func() error {
		rec, err := ReadLegacyRecord(stateDir)
		if err != nil || !rec.RetiredAt.Equal(from.UTC()) || !to.After(from) {
			return err
		}
		rec.RetiredAt = to.UTC()
		return config.WriteJSON(filepath.Join(stateDir, LegacyRecordFile), rec, 0o600)
	})
}

// MarkLegacyForgotten moves the record's ForgottenAt forward to at; never back, and never on
// a record not yet kept.
func MarkLegacyForgotten(stateDir string, at time.Time) error {
	return withLegacyRecord(stateDir, func() error {
		rec, err := ReadLegacyRecord(stateDir)
		if err != nil || rec.RetiredAt.IsZero() || !at.After(rec.ForgottenAt) {
			return err
		}
		rec.ForgottenAt = at.UTC()
		return config.WriteJSON(filepath.Join(stateDir, LegacyRecordFile), rec, 0o600)
	})
}

// RecordNoOne keeps, under stateDir, that the sign-ins an older terma left name no one
// whole (a file that parses to none, or not cleanly): what that terma queued until now is
// no one's. The sessions that could be read, signIns, are kept for the heartbeat all the
// same. A record already kept stands, and gains those sessions.
func RecordNoOne(stateDir string, signIns []LegacySignIn, now time.Time) error {
	return withLegacyRecord(stateDir, func() error { return recordNoOne(stateDir, signIns, now) })
}

func recordNoOne(stateDir string, signIns []LegacySignIn, now time.Time) error {
	rec, err := ReadLegacyRecord(stateDir)
	if err != nil {
		return err
	}
	if rec.RetiredAt.IsZero() {
		rec = LegacyRecord{Account: LegacyAccountAmbiguous, RetiredAt: now.UTC()}
	}
	for _, s := range signIns {
		if s.SessionID != "" && !slices.Contains(rec.SessionIDs, s.SessionID) {
			rec.SessionIDs = append(rec.SessionIDs, s.SessionID)
		}
	}
	return config.WriteJSON(filepath.Join(stateDir, LegacyRecordFile), rec, 0o600)
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
