package gitx

import (
	"errors"
	"os"
	"path/filepath"
)

// NestedWalkLimit bounds the folders NestedCheckoutsFS reads, the folder it starts from
// included. A variable so a test can lower it.
var NestedWalkLimit = 20000

// ErrNestedWalkBound is NestedCheckoutsFS's error when the walk would read more than
// NestedWalkLimit folders, the one it starts from included: what the rest holds is unknown.
var ErrNestedWalkBound = errors.New("gitx: too many folders to walk")

// NestedCheckoutsFS calls yield for each checkout nested below dir, an absolute folder
// ResolveFolderFS returned: each folder under it with a .git entry, as PlaceFolderFS
// recognises one, with its root and git directory, gitDir "" when LocateFS cannot read it.
// dir's own checkout is not one: PlaceFolderFS places dir. The walk is breadth first, reads
// at most NestedWalkLimit folders, never enters a .git folder, and never follows a link to a
// folder: a link's target is placed when a path names it, not here. A checkout nested in a
// nested one is yielded too. yield returning false stops the walk with no error.
//
// The error is ErrNestedWalkBound when the bound was hit, or the error of a folder that
// could not be read; the checkouts yielded before it stand. With GIT_DIR in the environment
// no checkout can be told (PlaceFolderFS), so it walks nothing and returns an error.
func NestedCheckoutsFS(dir string, yield func(root, gitDir string) bool) error {
	if os.Getenv("GIT_DIR") != "" {
		return errors.New("gitx: GIT_DIR is set")
	}
	if !filepath.IsAbs(dir) {
		return errors.New("gitx: a relative folder")
	}
	// queue[read] is the next folder to read; each one read is cleared, so the folders
	// already walked are not kept for the rest of the walk.
	queue := []string{filepath.Clean(dir)}
	for read := 0; read < len(queue); read++ {
		if read >= NestedWalkLimit {
			return ErrNestedWalkBound
		}
		cur := queue[read]
		queue[read] = ""
		entries, err := os.ReadDir(cur)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.Name() == ".git" {
				if read == 0 {
					continue // dir's own checkout
				}
				root, gitDir, ok := LocateFS(cur)
				if !ok || root != cur {
					gitDir = ""
				}
				if !yield(cur, gitDir) {
					return nil
				}
				continue
			}
			// IsDir is false for a link, whatever it leads to, and on Windows for a junction.
			if entry.IsDir() {
				queue = append(queue, filepath.Join(cur, entry.Name()))
			}
		}
	}
	return nil
}
