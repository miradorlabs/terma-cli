package auth

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// credentialIndex is one version of the credential store: its file in the config
// directory, its sidecar lock, and the names of its keychain items. Both hold the same
// shape, profile → organizations.
type credentialIndex int

const (
	// v1 is credentials.json and its credential/<profile>/<org> items, which every terma
	// before multi-org routing reads.
	v1 credentialIndex = iota + 1
	// v2 is credentials.v2.json and its credential.v2/<profile>/<org> items, which only a
	// migrated machine has and older binaries never read.
	v2
)

// indexOf is the one place the credential store picks its version: v2 on a migrated
// machine (config.Migrated), v1 otherwise.
func indexOf(dir string) credentialIndex {
	if config.Migrated(dir) {
		return v2
	}
	return v1
}

// path is x's file under the config directory dir.
func (x credentialIndex) path(dir string) string {
	if x == v2 {
		return config.CredentialsV2Path(dir)
	}
	return config.CredentialsPath(dir)
}

// item names the keychain item holding profile's tokens for organizationID in x.
func (x credentialIndex) item(profile, organizationID string) string {
	if x == v2 {
		return "credential.v2/" + profile + "/" + organizationID
	}
	return itemName(profile, organizationID)
}

// lockIndex locks the index the config directory dir uses and returns it. Only the
// migration's copy makes a machine migrated, and it holds v1's lock to do so, so an index
// still in use once its lock is held stays in use until it is released.
func lockIndex(ctx context.Context, dir string) (credentialIndex, func(), error) {
	for {
		x := indexOf(dir)
		unlock, err := lockCredentialFile(ctx, x.path(dir))
		if err != nil {
			return 0, nil, err
		}
		if indexOf(dir) == x {
			return x, unlock, nil
		}
		unlock()
	}
}
