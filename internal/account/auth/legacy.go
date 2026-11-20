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
}

type legacyCredential struct {
	AccessToken    string `json:"access_token,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	AuthURL        string `json:"auth_url,omitempty"`
	OrganizationID string `json:"organization_id,omitempty"`
	UserID         string `json:"user_id,omitempty"`
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
	// An unreadable file is still one to delete.
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
				AuthURL: c.AuthURL, AccessToken: c.AccessToken}
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

// HasLegacySignIns reports whether an older terma left sign-ins under dir.
func HasLegacySignIns(dir string) bool {
	_, err := os.Stat(config.LegacyCredentialsPath(dir))
	return err == nil
}

// ForgetLegacy deletes the sign-ins an older terma left: every keychain item
// credentials.json names, then the file and its lock. A keychain item that cannot be
// removed is reported, and the file stays so a later sign-in tries again.
func ForgetLegacy(dir string) error {
	signIns, err := LegacySignIns(dir, false)
	if err != nil {
		return err
	}
	var errs []error
	for _, s := range signIns {
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
// whose name what an older terma queued may still leave under. Account is "" when they
// named no single person and environment.
type LegacyRecord struct {
	SessionIDs []string  `json:"session_ids,omitempty"`
	Account    string    `json:"account,omitempty"`
	RetiredAt  time.Time `json:"retired_at"`
}

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
	rec, _ := ReadLegacyRecord(stateDir)
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
	if fresh && len(accounts) == 1 {
		for a := range accounts {
			rec.Account = a
		}
	}
	return config.WriteJSON(filepath.Join(stateDir, LegacyRecordFile), rec, 0o600)
}

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
