package keystore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/account/serverkey"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

// ErrMigrated is SetLegacy on a migrated machine: keys.json takes no key there.
var ErrMigrated = errors.New("keystore: this machine routes by teams, so keys.json takes no key")

// SetLegacy is Set for a key minted under the rules before the migration, as the relay's
// minter mints one while the migration runs beside it: refused with ErrMigrated once the
// machine is migrated (config.Migrated), which it asks under keys.json's lock. The
// migration marks the machine migrated before it copies and retires keys.json under that
// lock, so such a key lands before the copy, and moves or goes with the rest, or not at
// all: never in keys.json after it, where an older terma would send with it.
func SetLegacy(dir, projectID, key string, hosts Hosts) error {
	if projectID == "" || !serverkey.Is(key) {
		return errors.New("keystore: a team id and a server key are required")
	}
	return updateOr(dir, func(f *file, inFile bool) error {
		if config.Migrated(dir) {
			return ErrMigrated
		}
		f.recordHosts(dir, projectID, key, hosts)
		f.write(dir, f.Keys, projectID, projectItem(projectID), key, inFile)
		return nil
	})
}

// CopyLegacyToTeams is the migration's copy of keys: each project legacy maps (project →
// cutover enrollment) has its own key and hosts copied from keys.json into keys.v2.json
// under its enrollment. Harness keys are dropped, since a team sends with its own key
// and no other, and a project with no key of its own copies nothing. An enrollment
// keys.v2.json already holds is left as it is, so the copy is safe to repeat; keys.json
// is left as it is, for RetireV1. It runs under both files' locks, keys.json's first.
func CopyLegacyToTeams(dir string, legacy map[string]string) error {
	for project, enrollment := range legacy {
		if project == "" || enrollment == "" {
			return errors.New("keystore: a legacy project and its enrollment are required")
		}
	}
	inFile := config.InsecureStorage(dir)
	var added []string
	err := flock.Locked(path(dir), lockWait, func() error {
		v1, err := load(dir)
		if err != nil {
			return err
		}
		return flock.Locked(config.KeysV2Path(dir), lockWait, func() error {
			return editTeams(dir, inFile, func(f *teamsFile, inFile bool) error {
				for project, enrollment := range legacy {
					if have, ok := f.Teams[enrollment]; ok {
						if have.Project != project {
							return fmt.Errorf("keystore: enrollment %s holds team %s's key, not %s's", enrollment, have.Project, project)
						}
						continue
					}
					key, err := v1.read(dir, v1.Keys, project, projectItem(project))
					if err != nil {
						return err
					}
					if key != "" {
						f.put(dir, enrollment, project, key, v1.Hosts[project], inFile)
					}
				}
				return nil
			}, &added)
		})
	})
	if err != nil {
		for _, item := range added {
			_ = secret.Delete(dir, item)
		}
		return err
	}
	sweepTeams(dir)
	return nil
}

// RetireV1 is the migration's retirement of keys.json: its keys, harness keys and their
// hosts go, so an older terma finds no key to send with, and their keychain items are
// removed once the file no longer indexes them. An item the keychain would not remove
// stays marked stale and is reported; repeating RetireV1 removes it. keys.v2.json and
// its items are never touched.
func RetireV1(dir string) error {
	if _, err := os.Stat(path(dir)); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	err := update(dir, func(f *file, _ bool) {
		for id, v := range f.Keys {
			if v == "" {
				f.mark(projectItem(id))
			}
		}
		for harness, keys := range f.HarnessKeys {
			for id, v := range keys {
				if v == "" {
					f.mark(harnessItem(harness, id))
				}
			}
		}
		f.Keys, f.HarnessKeys, f.Hosts = map[string]string{}, map[string]map[string]string{}, map[string]Hosts{}
	})
	if err != nil {
		return err
	}
	f, err := load(dir)
	if err != nil {
		return err
	}
	if len(f.Stale) > 0 {
		return fmt.Errorf("keystore: the system keychain kept %d of keys.json's items; retiring again removes them", len(f.Stale))
	}
	return nil
}
