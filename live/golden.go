package live

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// Golden key sets are how the suite notices interface drift: the attribute
// names a harness emitted on a surface the last time someone looked. A key
// that disappears fails the test; a new one is reported so the matrix can grow.
// LIVE_UPDATE_GOLDEN=1 rewrites the files from what was observed.

func goldenPath(name string) string {
	dir := os.Getenv("TERMA_LIVE_GOLDEN")
	if dir == "" {
		dir = "golden"
	}
	return filepath.Join(dir, name+".json")
}

// CheckKeys compares the observed key set for a surface with its golden file.
// strict fails on a missing key; the newest build is checked strictly, older
// builds only report their differences, since the golden describes the newest.
func CheckKeys(t *testing.T, name string, observed map[string]string, strict bool) {
	t.Helper()
	keys := make([]string, 0, len(observed))
	for k := range observed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	path := goldenPath(name)
	if os.Getenv("LIVE_UPDATE_GOLDEN") == "1" {
		data, _ := json.MarshalIndent(keys, "", "  ")
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("golden %s written (%d keys)", name, len(keys))
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Logf("no golden for %s yet; observed keys: %v (run with LIVE_UPDATE_GOLDEN=1 to record)", name, keys)
		Note(name, "no golden recorded")
		return
	}
	var want []string
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatalf("golden %s: %v", name, err)
	}
	have := map[string]bool{}
	for _, k := range keys {
		have[k] = true
	}
	wanted := map[string]bool{}
	for _, k := range want {
		wanted[k] = true
	}
	var missing, added []string
	for _, k := range want {
		if !have[k] {
			missing = append(missing, k)
		}
	}
	for _, k := range keys {
		if !wanted[k] {
			added = append(added, k)
		}
	}
	if len(added) > 0 {
		t.Logf("%s: new keys since golden: %v", name, added)
		Note(name, "new keys: "+joinStrings(added))
	}
	if len(missing) > 0 {
		if strict {
			t.Errorf("%s: keys in golden but not observed (interface drift): %v", name, missing)
		} else {
			t.Logf("%s: keys in golden not observed on this build: %v", name, missing)
			Note(name, "older build lacks: "+joinStrings(missing))
		}
	}
}

func joinStrings(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}
