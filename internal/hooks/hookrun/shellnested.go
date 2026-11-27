package hookrun

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// maxNestedFolders bounds the folders listed to find the checkouts nested in one checkout;
// a checkout with more fails closed.
const maxNestedFolders = 200_000

// nestedIndex holds the checkouts nested in each checkout a past session searched, listed
// once per process: `terma history upload` scans many past searches of the same checkout,
// and each scan gives up below maxBelowDepth folders.
var nestedIndex = struct {
	sync.Mutex
	lists map[string]nestedList
}{lists: map[string]nestedList{}}

type nestedList struct {
	roots []string
	ok    bool
}

// placeNestedBelow places the checkouts nested in root (the checkout dir is in) below dir,
// and fails closed when they cannot all be listed. Listing them, or waiting while another
// scan does, is not charged to this scan's time: it is done once per checkout, for every
// past search of it.
func (sc *scan) placeNestedBelow(root, dir string) {
	start := time.Now()
	nestedIndex.Lock()
	l, listed := nestedIndex.lists[root]
	if !listed {
		l.roots, l.ok = listNested(root, sc.holdsNone, maxNestedFolders)
		nestedIndex.lists[root] = l
	}
	nestedIndex.Unlock()
	sc.deadline = sc.deadline.Add(time.Since(start))
	if !l.ok {
		sc.fail()
		return
	}
	for _, n := range l.roots {
		if under(n, dir) {
			sc.place(n)
		}
	}
}

// listNested lists the checkouts (folders holding a .git) below root, in no more than limit
// folders, skipping what skip names and every .git; false when root's folders cannot all be
// read or number more than limit. Like git and ripgrep, it does not follow symbolic links.
func listNested(root string, skip func(string) bool, limit int) ([]string, bool) {
	var roots []string
	queue, folders := []string{root}, 0
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) && dir != root {
			continue
		} else if err != nil {
			return nil, false
		}
		for _, e := range entries {
			if e.Name() == ".git" {
				if dir != root {
					roots = append(roots, dir)
				}
				continue
			}
			if !e.IsDir() {
				continue
			}
			if sub := filepath.Join(dir, e.Name()); !skip(sub) {
				if folders++; folders > limit {
					return nil, false
				}
				queue = append(queue, sub)
			}
		}
	}
	return roots, true
}
