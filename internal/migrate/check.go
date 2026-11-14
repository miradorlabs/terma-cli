package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/procinfo"
)

// otherInstalls are the termas the agents' hook entries run that are not the relay's own
// build (step 1): they would keep the retired rules. The same file, or a copy of the same
// build, is the relay's; a missing one runs nothing, as every entry is guarded. It also
// returns the relay's terma, for doctor.
func (m Machine) otherInstalls() (string, []string, error) {
	if m.Hooked == nil {
		return m.Executable, nil, nil
	}
	hooked, err := m.Hooked()
	if err != nil {
		return m.Executable, nil, err
	}
	exe := m.Executable
	var mine fs.FileInfo
	var build string
	var others []string
	for _, path := range hooked {
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return exe, nil, err
		}
		if mine == nil {
			if exe == "" {
				if exe, err = procinfo.AbsExecutable(); err != nil {
					return exe, nil, err
				}
			}
			if mine, err = os.Stat(exe); err != nil {
				return exe, nil, err
			}
		}
		if os.SameFile(info, mine) {
			continue
		}
		if build == "" {
			if build, err = fileDigest(exe); err != nil {
				return exe, nil, err
			}
		}
		if d, err := fileDigest(path); err != nil || d != build {
			others = append(others, path)
		}
	}
	slices.Sort(others)
	return exe, slices.Compact(others), nil
}

// fileDigest is the SHA-256 of the file at path.
func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
