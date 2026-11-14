package hookmgr

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// ManagedHookCommand is UserHookCommand for an organization's managed configuration, which
// serves every account: terma under $HOME, double-quoted so the shell expands it per user.
func ManagedHookCommand(terma string) func(event string) string {
	return userHookCommand(`"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`").Replace(terma) + `"`)
}

// HookedTerma lists, by path, the termas the machine-wide entries of the agent hooks file
// at path run: each "command" UserHookCommand wrote, read back, wherever the agent nests
// it. A file that does not exist lists none. One that cannot be read or parsed is an
// error: which terma its entries run is then not known.
func HookedTerma(path string) ([]string, error) {
	data, err := ReadFile(path)
	if err != nil || data == nil {
		return nil, err
	}
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for key, value := range t {
				if s, ok := value.(string); ok && key == "command" {
					if terma, _, ok := userHookTerma(strings.TrimSpace(s)); ok {
						out = append(out, terma)
					}
					continue
				}
				walk(value)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(doc)
	slices.Sort(out)
	return slices.Compact(out), nil
}
