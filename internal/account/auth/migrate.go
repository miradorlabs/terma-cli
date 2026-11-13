package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// CopyToV2 is the migration's copy of sign-ins. Every credential credentials.json
// indexes is copied, with its tokens, into credentials.v2.json and its own keychain items,
// under both files' locks. It always writes credentials.v2.json, which marks the machine
// migrated (config.Migrated): from then on every function here reads and writes v2. A
// credential v2 already holds is left as it is, since a refresh since the first copy
// rotated it there, so the copy is safe to repeat. credentials.json is left as it is, for
// RetireV1.
func CopyToV2(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	inFile := config.InsecureStorage(dir)
	var added []string
	err := lockedBoth(dir, func() error {
		old, err := loadCredentials(dir, v1)
		if err != nil {
			return err
		}
		file, err := loadCredentials(dir, v2)
		if err != nil {
			return err
		}
		for profile, p := range old {
			for org, c := range p.organizations() {
				if !c.present() || file[profile].organizations()[org].present() {
					continue
				}
				cred := *c
				switch err := hydrate(dir, v1, profile, &cred); {
				case errors.Is(err, errTokensGone):
					// Signed out in v1, so in v2: the index alone, without tokens.
					cred.StaleKeychainItem = false
				case err != nil:
					return err
				default:
					cred = *keep(dir, v2, profile, &cred, nil, inFile)
					if cred.AccessToken == "" {
						added = append(added, v2.item(profile, org))
					}
				}
				if file[profile] == nil {
					file[profile] = &profileCredentials{}
				}
				file[profile].put(&cred)
			}
			if q := file[profile]; q != nil && q.Active == "" && q.organizations()[p.Active].present() {
				q.Active = p.Active
			}
		}
		return saveCredentialFile(v2.path(dir), file)
	})
	if err != nil {
		for _, item := range added {
			_ = secret.Delete(dir, item)
		}
		return err
	}
	sweepStale(dir)
	return nil
}

// RetireV1 is the migration's retirement of credentials.json, once credentials.v2.json
// holds every sign-in it does. Their keychain items are removed first, then the file's
// organizations are emptied, so an older terma finds a signed-out machine and no item is
// left that nothing indexes. It runs under both files' locks. A keychain that will not
// remove an item fails it with credentials.json as it was; it is safe to repeat.
func RetireV1(dir string) error {
	return lockedBoth(dir, func() error {
		old, err := loadCredentials(dir, v1)
		if err != nil || len(old) == 0 {
			return err
		}
		file, err := loadCredentials(dir, v2)
		if err != nil {
			return err
		}
		var missing []string
		var errs []error
		for profile, p := range old {
			for org, c := range p.organizations() {
				if c.present() && !file[profile].organizations()[org].present() {
					missing = append(missing, profile+"/"+org)
				}
			}
		}
		if len(missing) > 0 {
			slices.Sort(missing)
			return fmt.Errorf("credentials.v2.json does not hold %s yet: copy the sign-ins before retiring them", strings.Join(missing, ", "))
		}
		for profile, p := range old {
			for _, c := range p.organizations() {
				if c.present() && (c.AccessToken == "" || c.StaleKeychainItem) {
					if err := secret.Delete(dir, v1.item(profile, c.OrganizationID)); err != nil {
						errs = append(errs, err)
					}
				}
			}
		}
		if len(errs) > 0 {
			return fmt.Errorf("could not remove %d legacy sign-ins from the system keychain: %w", len(errs), errors.Join(errs...))
		}
		changed := false
		for _, p := range old {
			if p != nil && (len(p.Organizations) > 0 || p.Active != "") {
				p.Organizations, p.Active, changed = map[string]*Credential{}, "", true
			}
		}
		if !changed {
			return nil
		}
		return saveCredentialFile(v1.path(dir), old)
	})
}

// lockedBoth runs fn holding v1's lock and then v2's: the order every holder of both
// takes them in, so two never wait on each other.
func lockedBoth(dir string, fn func() error) error {
	ctx, cancel := context.WithTimeout(context.Background(), lockWait)
	defer cancel()
	unlockV1, err := lockCredentialFile(ctx, v1.path(dir))
	if err != nil {
		return err
	}
	defer unlockV1()
	unlockV2, err := lockCredentialFile(ctx, v2.path(dir))
	if err != nil {
		return err
	}
	defer unlockV2()
	return fn()
}
