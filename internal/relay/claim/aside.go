package claim

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// Lock-free <claim>.mixed/<rand>.json sidecar for KindMixed writes whose lock timed out.
// Read merges the sidecar before reading <claim>; locked writes save <claim> before removing marks.
const (
	asideExt    = ".mixed"
	asideWrites = 3
)

type asideMark struct {
	claim Claim
	file  string
}

func asidePath(claimPath string) string { return claimPath + asideExt }

func writeMixedAside(claimPath string, c Claim, now time.Time) bool {
	c.Kind, c.ProjectID, c.ClaimedAt = KindMixed, "", now.UTC()
	if existing, _ := loadAside(claimPath, c.ClaimedAt); len(existing) > 0 {
		for _, em := range existing {
			e := em.claim
			if e.Run == c.Run && e.Account == c.Account && e.Profile == c.Profile &&
				subset(c.PIDs, e.PIDs) && !c.ClaimedAt.Before(e.ClaimedAt) {
				_ = os.Chtimes(filepath.Join(asidePath(claimPath), em.file), c.ClaimedAt, c.ClaimedAt)
				return true
			}
		}
	}
	data, err := json.Marshal(c)
	if err != nil {
		return false
	}
	var rnd [16]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return false
	}
	dir := asidePath(claimPath)
	file := filepath.Join(dir, hex.EncodeToString(rnd[:])+".json")
	for range asideWrites {
		if err = os.MkdirAll(dir, 0o700); err == nil {
			if err = config.WriteFileAtomicNoSync(file, data, 0o600); err == nil {
				return true
			}
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false
		}
	}
	return false
}

// loadAside reads unexpired KindMixed sidecar marks for claimPath, sorted by ClaimedAt.
// Expired filenames are returned in files for cleanup; corrupt unexpired files fail closed.
func loadAside(claimPath string, now time.Time) ([]asideMark, []string) {
	dir := asidePath(claimPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	var marks []asideMark
	var files []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), config.TempPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) >= TTL {
			files = append(files, e.Name())
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		var m Claim
		if err != nil || json.Unmarshal(data, &m) != nil || m.Kind != KindMixed {
			m = Claim{Kind: KindMixed} // nil PIDs covers all processes; zero time poisons the run
		}
		m.ProjectID = ""
		marks, files = append(marks, asideMark{claim: m, file: e.Name()}), append(files, e.Name())
	}
	slices.SortStableFunc(marks, func(a, b asideMark) int { return a.claim.ClaimedAt.Compare(b.claim.ClaimedAt) })
	return marks, files
}

// applyAside folds unexpired sidecar KindMixed marks onto prev (if any).
func applyAside(prev Claim, havePrev bool, marks []asideMark) (Claim, bool, bool) {
	if len(marks) == 0 {
		return prev, havePrev, false
	}
	changed := false
	for _, em := range marks {
		m := em.claim
		m.Kind, m.ProjectID = KindMixed, ""
		if !havePrev {
			m.Placements = []Placement{{Kind: KindMixed, Run: m.Run, Profile: m.Profile, Account: m.Account, Tool: m.Tool, PIDs: slices.Clone(m.PIDs), Since: m.ClaimedAt}}
			prev, havePrev, changed = m, true, true
			continue
		}
		sameRun := m.Run == "" || prev.Run == "" || m.Run == prev.Run
		if !sameRun {
			if matched, ch := foldEarlierRun(&prev, m); matched {
				changed = changed || ch
				continue
			}
		}
		sameLane := sameRun && (m.Profile == "" || m.Profile == prev.Profile) && (m.Account == "" || m.Account == prev.Account)
		claimedAt := prev.ClaimedAt
		if m.ClaimedAt.After(claimedAt) {
			claimedAt = m.ClaimedAt
		}
		if prev.Kind == KindMixed && sameLane && len(m.PIDs) > 0 {
			prev.PIDs, prev.ClaimedAt = merge(prev.PIDs, m.PIDs), claimedAt
			prev.Run, prev.Parent = cmp.Or(prev.Run, m.Run), cmp.Or(prev.Parent, m.Parent)
			pls := slices.Clone(prev.placements())
			pls[len(pls)-1].PIDs, pls[len(pls)-1].Run = prev.PIDs, prev.Run
			prev.Placements, changed = pls, true
			continue
		}
		pls, pids := slices.Clone(prev.placements()), slices.Clone(m.PIDs)
		if sameLane && len(m.PIDs) > 0 {
			pids = merge(prev.PIDs, m.PIDs)
		}
		if sameRun {
			poisonRun(pls, m.Run, m.ClaimedAt)
		}
		since := m.ClaimedAt
		if len(pls) > 0 && since.Before(pls[len(pls)-1].Since) {
			since = pls[len(pls)-1].Since
		}
		runID, profile, account, tool, parent := cmp.Or(m.Run, prev.Run), cmp.Or(m.Profile, prev.Profile), cmp.Or(m.Account, prev.Account), cmp.Or(m.Tool, prev.Tool), cmp.Or(prev.Parent, m.Parent)
		pls = append(pls, Placement{Kind: KindMixed, Run: runID, Profile: profile, Account: account, Tool: tool, PIDs: pids, Since: since})
		evicted := prev.Evicted
		if len(pls) > maxPlacements {
			pls, evicted = pls[len(pls)-maxPlacements:], true
		}
		prev, changed = Claim{Kind: KindMixed, Run: runID, Profile: profile, Account: account, Tool: tool, Parent: parent, PIDs: pids, Placements: pls, Evicted: evicted, ClaimedAt: claimedAt}, true
	}
	return prev, havePrev, changed
}

func poisonRun(pls []Placement, run string, since time.Time) bool {
	hit := false
	for i := range pls {
		if (run == "" || pls[i].Run == "" || pls[i].Run == run) && !pls[i].Since.Before(since) && (pls[i].Kind != KindMixed || pls[i].ProjectID != "") {
			pls[i].Kind, pls[i].ProjectID, hit = KindMixed, "", true
		}
	}
	return hit
}

func foldEarlierRun(prev *Claim, m Claim) (matched, changed bool) {
	pls, idx := slices.Clone(prev.placements()), -1
	for i := len(pls) - 1; i >= 0; i-- {
		if pls[i].Run == m.Run {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false, false
	}
	sameLane := (m.Profile == "" || pls[idx].Profile == m.Profile) && (m.Account == "" || pls[idx].Account == m.Account)
	if pls[idx].Kind == KindMixed && sameLane {
		mod := false
		if m.ClaimedAt.Before(pls[idx].Since) {
			mod = poisonRun(pls[:idx], m.Run, m.ClaimedAt)
			since := m.ClaimedAt
			if idx > 0 && since.Before(pls[idx-1].Since) {
				since = pls[idx-1].Since
			}
			if since.Before(pls[idx].Since) {
				pls[idx].Since, mod = since, true
			}
		}
		if len(pls[idx].PIDs) > 0 {
			if len(m.PIDs) == 0 {
				pls[idx].PIDs, mod = nil, true
			} else if !subset(m.PIDs, pls[idx].PIDs) {
				pls[idx].PIDs, mod = merge(pls[idx].PIDs, m.PIDs), true
			}
		}
		if mod {
			prev.Placements = pls
		}
		return true, mod
	}
	poisonRun(pls[:idx+1], m.Run, m.ClaimedAt)
	pids := slices.Clone(m.PIDs)
	if len(m.PIDs) > 0 && sameLane {
		pids = merge(pls[idx].PIDs, m.PIDs)
	}
	since := m.ClaimedAt
	if since.Before(pls[idx].Since) {
		since = pls[idx].Since
	}
	if idx+1 < len(pls) && since.After(pls[idx+1].Since) {
		since = pls[idx+1].Since
	}
	pls = slices.Insert(pls, idx+1, Placement{Kind: KindMixed, Run: m.Run, Profile: cmp.Or(m.Profile, pls[idx].Profile), Account: cmp.Or(m.Account, pls[idx].Account), Tool: cmp.Or(m.Tool, pls[idx].Tool), PIDs: pids, Since: since})
	if len(pls) > maxPlacements {
		pls, prev.Evicted = pls[len(pls)-maxPlacements:], true
	}
	prev.Placements = pls
	return true, true
}

func removeAside(claimPath string, files []string) {
	dir := asidePath(claimPath)
	for _, name := range files {
		_ = os.Remove(filepath.Join(dir, name))
	}
	_ = os.Remove(dir)
}

func pruneAside(dir string, now time.Time) {
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) >= TTL {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
		_ = os.Remove(dir)
	}
}
