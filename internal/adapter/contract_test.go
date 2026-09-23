package adapter

import (
	"regexp"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// hookCommand finds the event a committed hook entry runs. The files differ — JSON with
// the command under three different shapes — and the text `terma hook <event>` is what
// they all share, which is also all that reaches `terma hook` at run time.
var hookCommand = regexp.MustCompile(`terma hook ([a-z][a-z-]*)`)

// Event names are written twice: in hookmgr, into the files customers commit, and in
// each adapter's Events, where `terma hook <event>` looks them up. Nothing tied the two
// lists together, and the failure is silent on every side — a committed hook naming an
// event with no handler does nothing, for ever, in every repository that ran install.
func TestEveryCommittedHookHasAHandler(t *testing.T) {
	handlers := Handlers()
	for _, a := range All() {
		if a.HooksPath() == "" {
			continue
		}
		plan, err := a.Plan(t.TempDir(), true)
		if err != nil {
			t.Fatalf("%s: %v", a.Name(), err)
		}
		own := a.Events()
		found := 0
		for _, change := range plan.Changes {
			for _, m := range hookCommand.FindAllStringSubmatch(string(change.After), -1) {
				found++
				event := m[1]
				if _, ok := handlers[event]; !ok {
					t.Errorf("%s commits `terma hook %s`, which no adapter handles", a.Name(), event)
				} else if _, ok := own[event]; !ok {
					t.Errorf("%s commits `terma hook %s`, which belongs to another adapter", a.Name(), event)
				}
			}
		}
		if found == 0 {
			t.Errorf("%s writes %s with no `terma hook` command in it", a.Name(), a.HooksPath())
		}
	}
}

// harness.SupportCatalog is what `terma harness` prints, and it names the agents again
// by hand. An adapter added to the registry and not to the catalog would be supported
// and unlisted; one renamed in a single place would be listed under a name install
// rejects.
func TestSupportCatalogMatchesTheRegistry(t *testing.T) {
	listed := map[string]string{}
	for _, agent := range harness.SupportCatalog() {
		listed[agent.Name] = agent.DisplayName
	}
	for _, a := range All() {
		name, ok := listed[a.Name()]
		if !ok {
			t.Errorf("adapter %q is missing from harness.SupportCatalog", a.Name())
			continue
		}
		if name != a.DisplayName() {
			t.Errorf("%s: the catalog calls it %q, the adapter %q", a.Name(), name, a.DisplayName())
		}
		delete(listed, a.Name())
	}
	for name := range listed {
		t.Errorf("harness.SupportCatalog lists %q, which is not an adapter", name)
	}
}
