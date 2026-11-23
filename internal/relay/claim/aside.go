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

// Lock-free <claim>.mixed/<rand>.json sidecar for KindMixed writes on lock timeout.
// Read merges before reading <claim>; locked writes save <claim> before removing marks.
const asideExt, asideWrites = ".mixed", 3

type asideMark struct {
	claim Claim
	file  string
}

func asidePath(claimPath string) string { return claimPath + asideExt }

func writeMixedAside(claimPath string, c Claim, now time.Time) bool {
	c.Kind, c.ProjectID, c.ClaimedAt = KindMixed, "", now.UTC()
	existing, _ := loadAside(claimPath, c.ClaimedAt)
	for _, em := range existing {
		e := em.claim
		if em.file != "" && e.Run == c.Run && e.Account == c.Account && e.Profile == c.Profile &&
			coveredPIDs(c.PIDs, e.PIDs) && !c.ClaimedAt.Before(e.ClaimedAt) {
			_ = os.Chtimes(filepath.Join(asidePath(claimPath), em.file), c.ClaimedAt, c.ClaimedAt)
			return true
		}
	}
	data, err := json.Marshal(c)
	if err != nil {
		return false
	}
	var rnd [16]byte
	_, _ = rand.Read(rnd[:])
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

func loadAside(claimPath string, now time.Time) ([]asideMark, []string) {
	dir := asidePath(claimPath)
	var entries []os.DirEntry
	var err error
	for range 2 {
		if entries, err = os.ReadDir(dir); err == nil {
			break
		}
		info, statErr := os.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) && errors.Is(statErr, fs.ErrNotExist) {
			return nil, nil
		}
		if !errors.Is(err, fs.ErrNotExist) || statErr != nil || !info.IsDir() {
			return []asideMark{{claim: Claim{Kind: KindMixed}}}, nil
		}
	}
	if err != nil {
		return []asideMark{{claim: Claim{Kind: KindMixed}}}, nil
	}
	marks, files := []asideMark(nil), []string(nil)
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), config.TempPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				marks = append(marks, asideMark{claim: Claim{Kind: KindMixed}})
			}
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
			m = Claim{Kind: KindMixed}
		}
		m.ProjectID = ""
		marks, files = append(marks, asideMark{claim: m, file: e.Name()}), append(files, e.Name())
	}
	slices.SortStableFunc(marks, func(a, b asideMark) int { return a.claim.ClaimedAt.Compare(b.claim.ClaimedAt) })
	return marks, files
}

func applyAside(prev Claim, havePrev bool, marks []asideMark) (Claim, bool, bool) {
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
		pls := slices.Clone(prev.placements())
		if prev.Kind == KindMixed && sameLane {
			var mod bool
			pls, mod = mergeMixedInto(pls, len(pls)-1, m)
			last := len(pls) - 1
			if mod && len(prev.PIDs) > 0 {
				prev.PIDs = pls[last].PIDs
			}
			if prev.Run == "" && m.Run != "" {
				prev.Run, pls[last].Run, mod = m.Run, m.Run, true
			}
			if prev.Parent == "" && m.Parent != "" {
				prev.Parent, mod = m.Parent, true
			}
			if mod || claimedAt.After(prev.ClaimedAt) {
				prev.ClaimedAt, prev.Placements, changed = claimedAt, pls, true
			}
			continue
		}
		pids := slices.Clone(m.PIDs)
		if sameRun {
			pls, pids, _ = prepareMixed(pls, m.Run, m.Profile, m.Account, m.PIDs, m.ClaimedAt)
		}
		since := clampSince(pls, m.Run, m.ClaimedAt)
		runID, profile, account, tool, parent := cmp.Or(m.Run, prev.Run), cmp.Or(m.Profile, prev.Profile), cmp.Or(m.Account, prev.Account), cmp.Or(m.Tool, prev.Tool), cmp.Or(prev.Parent, m.Parent)
		pls = append(pls, Placement{Kind: KindMixed, Run: runID, Profile: profile, Account: account, Tool: tool, PIDs: pids, Since: since})
		pls, evicted, mixed := trimPlacements(pls, prev.Evicted, prev.MixedRuns)
		prev = Claim{Kind: KindMixed, Run: runID, Profile: profile, Account: account, Tool: tool, Parent: parent, PIDs: pids, Placements: pls, Evicted: evicted, MixedRuns: mixed, ClaimedAt: claimedAt}
		changed = true
	}
	return prev, havePrev, changed
}

func clampSince(pls []Placement, run string, since time.Time) time.Time {
	for _, pl := range pls {
		if (run == "" || pl.Run == "" || pl.Run != run) && since.Before(pl.Since) {
			since = pl.Since
		}
	}
	return since
}

// unionPIDs returns the union of a and b, or nil (all processes) if either is empty or > maxPIDs.
func unionPIDs(a, b []int) []int {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	out := slices.Clone(a)
	for _, v := range b {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	if len(out) > maxPIDs {
		return nil
	}
	return out
}

func coveredPIDs(want, have []int) bool { return len(have) == 0 || len(want) > 0 && subset(want, have) }

func mergeMixedInto(pls []Placement, idx int, m Claim) ([]Placement, bool) {
	pl, mod, cut := pls[idx], false, idx
	if !coveredPIDs(m.PIDs, pl.PIDs) {
		pl.PIDs, mod = unionPIDs(pl.PIDs, m.PIDs), true
	}
	if m.ClaimedAt.Before(pl.Since) {
		prefix, merged, hit := prepareMixed(pls[:idx], m.Run, m.Profile, m.Account, pl.PIDs, m.ClaimedAt)
		if !coveredPIDs(merged, pl.PIDs) {
			pl.PIDs, mod = merged, true
		}
		cut, mod = len(prefix), mod || hit
		if since := clampSince(prefix, m.Run, m.ClaimedAt); since.Before(pl.Since) {
			pl.Since, mod = since, true
		}
	}
	if !mod && cut == idx {
		return pls, false
	}
	return slices.Replace(pls, cut, idx+1, pl), true
}

func prepareMixed(pls []Placement, run, profile, account string, add []int, since time.Time) ([]Placement, []int, bool) {
	pids := slices.Clone(add)
	for _, pl := range pls {
		if len(pids) == 0 {
			break
		}
		if (run != "" && pl.Run != "" && pl.Run != run) || (profile != "" && pl.Profile != profile) || (account != "" && pl.Account != account) {
			continue
		}
		pids = unionPIDs(pids, pl.PIDs)
	}
	hit := poisonRun(pls, run, since)
	n := len(pls)
	for n > 0 {
		pl := pls[n-1]
		if (run != "" && pl.Run != "" && pl.Run != run) || (profile != "" && pl.Profile != profile) || (account != "" && pl.Account != account) || pl.Since.Before(since) {
			break
		}
		n--
	}
	return pls[:n], pids, hit
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
	for i := len(pls) - 1; i >= 0 && idx < 0; i-- {
		if pls[i].Run == m.Run {
			idx = i
		}
	}
	if idx < 0 {
		return slices.Contains(prev.MixedRuns, m.Run), false
	}
	sameLane := (m.Profile == "" || pls[idx].Profile == m.Profile) && (m.Account == "" || pls[idx].Account == m.Account)
	if pls[idx].Kind == KindMixed && sameLane {
		if next, mod := mergeMixedInto(pls, idx, m); mod {
			prev.Placements = next
			return true, true
		}
		return true, false
	}
	prefix, pids, _ := prepareMixed(pls[:idx+1], m.Run, m.Profile, m.Account, m.PIDs, m.ClaimedAt)
	since := clampSince(prefix, m.Run, m.ClaimedAt)
	if idx+1 < len(pls) && since.After(pls[idx+1].Since) {
		since = pls[idx+1].Since
	}
	pls = slices.Replace(pls, len(prefix), idx+1, Placement{Kind: KindMixed, Run: m.Run, Profile: cmp.Or(m.Profile, pls[idx].Profile), Account: cmp.Or(m.Account, pls[idx].Account), Tool: cmp.Or(m.Tool, pls[idx].Tool), PIDs: pids, Since: since})
	prev.Placements, prev.Evicted, prev.MixedRuns = trimPlacements(pls, prev.Evicted, prev.MixedRuns)
	return true, true
}

func trimPlacements(pls []Placement, evicted bool, mixed []string) ([]Placement, bool, []string) {
	if len(pls) <= maxPlacements {
		return pls, evicted, mixed
	}
	drop, out := len(pls)-maxPlacements, slices.Clone(mixed)
	for _, p := range pls[:drop] {
		if p.Kind != KindMixed || slices.Contains(out, "*") || slices.Contains(out, p.Run) {
			continue
		}
		if len(out) >= maxPIDs {
			out = []string{"*"}
		} else {
			out = append(out, p.Run)
		}
	}
	return pls[drop:], true, out
}

func removeAside(claimPath string, files []string) {
	if len(files) == 0 {
		return
	}
	dir := asidePath(claimPath)
	for _, name := range files {
		_ = os.Remove(filepath.Join(dir, name))
	}
	_ = os.Remove(dir)
}

func pruneAside(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) >= TTL {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	_ = os.Remove(dir)
}
