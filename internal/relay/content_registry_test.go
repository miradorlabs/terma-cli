package relay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The live suite's field registry (live/golden/relay/<harness>-fields.json) classifies
// every field that reaches Terma through the relay, from real harness runs. A field it
// calls content must be one this relay removes, and one it calls redacted one it blanks —
// otherwise a field someone classified as content would still leave a project that
// withholds it. This holds the registry and the relay's field sets together.
func TestFieldRegistryMatchesTheRelaysFieldSets(t *testing.T) {
	paths, _ := filepath.Glob(filepath.Join("..", "..", "live", "golden", "relay", "*-fields.json"))
	if len(paths) == 0 {
		t.Fatal("no field registry found under live/golden/relay")
	}
	removes := slices.Concat(toolContentFields, promptDropFields)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var reg map[string]string
		if err := json.Unmarshal(data, &reg); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for field, class := range reg {
			kind, key, _ := strings.Cut(field, ":")
			switch class {
			case "content":
				ok := false
				switch kind {
				case "log", "span", "span-event-attr":
					ok = slices.Contains(removes, key)
				case "span-event":
					ok = slices.Contains(toolContentEvents, key)
				case "log-body":
					ok = slices.Contains(promptBodyEvents, key)
				}
				if !ok {
					t.Errorf("%s: %s is content, but the relay does not remove it", filepath.Base(path), field)
				}
			case "redacted":
				if !slices.Contains(promptFields, key) {
					t.Errorf("%s: %s is redacted, but the relay does not blank it", filepath.Base(path), field)
				}
			case "metadata", "consent":
			default:
				t.Errorf("%s: %s has an unknown class %q", filepath.Base(path), field, class)
			}
		}
	}
}
