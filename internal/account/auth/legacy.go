package auth

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"

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
