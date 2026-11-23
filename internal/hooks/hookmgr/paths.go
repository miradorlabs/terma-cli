package hookmgr

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/tailscale/hujson"
)

// TermaPaths are the terma executables the machine-wide entries in a hooks file run, in the
// order they first appear: what each UserHookCommand in it names. The file may be JSONC;
// one that does not parse names none.
func TermaPaths(data []byte) []string {
	std, err := hujson.Standardize(data)
	if err != nil {
		return nil
	}
	var doc any
	if json.Unmarshal(std, &doc) != nil {
		return nil
	}
	var paths []string
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			if m := userHookParts.FindStringSubmatch(strings.TrimSpace(t)); m != nil && m[1] == m[2] {
				p := strings.ReplaceAll(m[1][1:len(m[1])-1], `'\''`, "'")
				if !slices.Contains(paths, p) {
					paths = append(paths, p)
				}
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, k := range keys {
				walk(t[k])
			}
		}
	}
	walk(doc)
	return paths
}
