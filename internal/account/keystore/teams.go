package keystore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/account/serverkey"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

// teamsFile is keys.v2.json, the machine-wide index of each selected team's key, by
// enrollment: re-ticking a team is a new enrollment with its own key, so deleting an
// ended enrollment's key never touches a newer one's. Older binaries never read it.
type teamsFile struct {
	Teams map[string]teamEntry `json:"teams"`
	// Stale names keychain items that may still hold a key the file no longer indexes
	// there; sweepTeams removes them once the file is on disk.
	Stale []string `json:"stale_keychain_items,omitempty"`
	// added are the items this write gave a key the file on disk does not index there.
	added []string
}

// teamEntry is one enrollment's key. As in keys.json, an empty Value means the keychain
// holds it.
type teamEntry struct {
	// Project is the team's project ID.
	Project string `json:"project"`
	Value   string `json:"value"`
	// Hosts are the key's environment, which alone accepts it.
	Hosts Hosts `json:"hosts,omitzero"`
}

// teamItem names an enrollment's keychain item: a prefix v1's keys never use, so its
// sweep never touches one.
func teamItem(enrollmentID string) string { return "team-key/" + enrollmentID }

func (f *teamsFile) mark(item string) {
	if !slices.Contains(f.Stale, item) {
		f.Stale = append(f.Stale, item)
	}
}

// live reports whether item holds a key the file indexes there now.
func (f *teamsFile) live(item string) bool {
	for id, e := range f.Teams {
		if e.Value == "" && teamItem(id) == item {
			return true
		}
	}
	return false
}

// read returns enrollmentID's key: from the file, else its keychain item; "" when there
// is none, and an error only when the keychain cannot be reached.
func (f *teamsFile) read(dir, enrollmentID string) (string, error) {
	e, ok := f.Teams[enrollmentID]
	if !ok {
		return "", nil
	}
	v := e.Value
	if v == "" {
		var err error
		if v, err = secret.Get(dir, teamItem(enrollmentID)); errors.Is(err, secret.ErrNotFound) {
			return "", nil
		} else if err != nil {
			return "", err
		}
	}
	if !serverkey.Is(v) {
		return "", nil
	}
	return v, nil
}

// put files key for enrollmentID under the rules of keys.json (file.write): where its
// earlier key lives, else the keychain unless inFile; a keychain that refuses it gets it
// in the file. A key already on file keeps the hosts it came with.
func (f *teamsFile) put(dir, enrollmentID, projectID, key string, hosts Hosts, inFile bool) {
	item := teamItem(enrollmentID)
	old, ok := f.Teams[enrollmentID]
	e := teamEntry{Project: projectID, Hosts: old.Hosts}
	if hosts = hosts.normalized(); hosts != (Hosts{}) {
		if prev, err := f.read(dir, enrollmentID); old.Hosts == (Hosts{}) || prev != key && !secret.IsUnavailable(err) {
			e.Hosts = hosts
		}
	}
	if ok && old.Value != "" {
		inFile = true
	}
	if !inFile {
		err := secret.Set(dir, item, key)
		if err == nil {
			f.Teams[enrollmentID] = e
			f.Stale = slices.DeleteFunc(f.Stale, func(s string) bool { return s == item })
			if !ok {
				f.added = append(f.added, item)
			}
			return
		}
		if secret.MayLand(err) {
			f.mark(item)
		}
	}
	if ok && old.Value == "" {
		f.mark(item)
	}
	e.Value = key
	f.Teams[enrollmentID] = e
}

func loadTeams(dir string) (*teamsFile, error) {
	data, err := os.ReadFile(config.KeysV2Path(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return &teamsFile{Teams: map[string]teamEntry{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var f teamsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", config.KeysV2Path(dir), err)
	}
	if f.Teams == nil {
		f.Teams = map[string]teamEntry{}
	}
	return &f, nil
}

// updateTeams is the one way keys.v2.json changes, under its own lock. A keychain item
// the edit filled for an entry the file never came to index is taken back.
func updateTeams(dir string, edit func(f *teamsFile, inFile bool) error) error {
	inFile := config.InsecureStorage(dir)
	var added []string
	err := flock.Locked(config.KeysV2Path(dir), lockWait, func() error {
		return editTeams(dir, inFile, edit, &added)
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

// editTeams is updateTeams' work once the lock is held.
func editTeams(dir string, inFile bool, edit func(f *teamsFile, inFile bool) error, added *[]string) error {
	f, err := loadTeams(dir)
	if err != nil {
		return err
	}
	err = edit(f, inFile)
	*added = f.added
	if err != nil {
		return err
	}
	return config.WriteJSON(config.KeysV2Path(dir), f, 0o600)
}

// sweepTeams removes the keychain items marked stale, under the file's lock, and unmarks
// each one it removes.
func sweepTeams(dir string) {
	if f, err := loadTeams(dir); err != nil || len(f.Stale) == 0 {
		return
	}
	_ = flock.Locked(config.KeysV2Path(dir), lockWait, func() error {
		f, err := loadTeams(dir)
		if err != nil {
			return err
		}
		var kept []string
		for _, item := range f.Stale {
			if !f.live(item) && secret.Delete(dir, item) != nil {
				kept = append(kept, item)
			}
		}
		if len(kept) == len(f.Stale) {
			return nil
		}
		f.Stale = kept
		return config.WriteJSON(config.KeysV2Path(dir), f, 0o600)
	})
}

// GetTeam returns an enrollment's team key from keys.v2.json under the config directory
// dir, and the hosts of the environment it belongs to; "" when it has none. An error
// means the index or the keychain could not be read: the key may well exist, so callers
// neither create another nor give up on it.
func GetTeam(dir, enrollmentID string) (key string, hosts Hosts, err error) {
	f, err := loadTeams(dir)
	if err != nil {
		return "", Hosts{}, err
	}
	key, err = f.read(dir, enrollmentID)
	if err != nil || key == "" {
		return "", Hosts{}, err
	}
	return key, f.Teams[enrollmentID].Hosts.resolved(), nil
}

// SetTeam records an enrollment's team key, for the team projectID, with the hosts of
// the environment it was created in. It lives in the keychain as team-key/<enrollment>,
// or in the file under InsecureStorage or when the keychain refuses it. An enrollment
// is one team: a key for another project is refused.
func SetTeam(dir, enrollmentID, projectID, key string, hosts Hosts) error {
	if enrollmentID == "" || projectID == "" || !serverkey.Is(key) {
		return errors.New("keystore: an enrollment, a team id and a server key are required")
	}
	return updateTeams(dir, func(f *teamsFile, inFile bool) error {
		if have, ok := f.Teams[enrollmentID]; ok && have.Project != projectID {
			return fmt.Errorf("keystore: enrollment %s holds team %s's key, not %s's", enrollmentID, have.Project, projectID)
		}
		f.put(dir, enrollmentID, projectID, key, hosts, inFile)
		return nil
	})
}

// DeleteTeam forgets an enrollment's team key, and removes its keychain item and no
// other once the file no longer indexes it.
func DeleteTeam(dir, enrollmentID string) error {
	return updateTeams(dir, func(f *teamsFile, _ bool) error {
		if e, ok := f.Teams[enrollmentID]; ok && e.Value == "" {
			f.mark(teamItem(enrollmentID))
		}
		delete(f.Teams, enrollmentID)
		return nil
	})
}
