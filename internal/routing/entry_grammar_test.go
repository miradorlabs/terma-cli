package routing

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// grammarCase is one row of testdata/entry_grammar.json, which the platform's account
// service tests too: how the server stores the entry, and how ParseEntry reads it, each
// exact, wildcard or refused.
type grammarCase struct {
	Entry  string `json:"entry"`
	Server string `json:"server"`
	CLI    string `json:"cli"`
}

// TestEntryGrammarSharedWithServer reads every entry of the table the account service
// shares. ParseEntry's reading is the table's, with wildcards on and, for an entry with no
// `*`, off too, as a 1.x policy is read. The contract holds between the two columns:
// nothing the server stores is refused here, and it is a wildcard to both or to neither.
func TestEntryGrammarSharedWithServer(t *testing.T) {
	raw, err := os.ReadFile("testdata/entry_grammar.json")
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Cases []grammarCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatal(err)
	}
	if len(table.Cases) == 0 {
		t.Fatal("the table has no cases")
	}
	kinds := map[string]bool{"exact": true, "wildcard": true, "refused": true}
	seen := map[string]bool{}
	for _, c := range table.Cases {
		if !kinds[c.Server] || !kinds[c.CLI] {
			t.Errorf("%q: server %q and cli %q must each be exact, wildcard or refused", c.Entry, c.Server, c.CLI)
		}
		// The server compares the entries it stores ignoring case, and the platform's integration
		// test saves them in one policy: two stored rows that differ only in case are one.
		dup := c.Entry
		if c.Server != "refused" {
			dup = "stored:" + strings.ToLower(c.Entry)
		}
		if seen[dup] {
			t.Errorf("%q is listed twice", c.Entry)
		}
		seen[dup] = true
		if got := reading(c.Entry, true); got != c.CLI {
			t.Errorf("ParseEntry(%q) reads %s, the table says %s", c.Entry, got, c.CLI)
		}
		if !strings.Contains(c.Entry, "*") {
			if got, with := reading(c.Entry, false), reading(c.Entry, true); got != with {
				t.Errorf("ParseEntry(%q) without wildcards reads %s, with them %s", c.Entry, got, with)
			}
		}
		if c.Server != "refused" && c.CLI == "refused" {
			t.Errorf("%q: the server stores it, so the CLI must read it", c.Entry)
		}
		if c.Server != "refused" && (c.Server == "wildcard") != (c.CLI == "wildcard") {
			t.Errorf("%q: the server reads it as %s and the CLI as %s", c.Entry, c.Server, c.CLI)
		}
		// config.Policy.Admits still decides for the callers not yet on routing.Route: a stored
		// exact entry admits the repository it names, and a wildcard, read as 1.x, admits nothing.
		switch e, ok := ParseEntry(c.Entry, true); {
		case c.Server == "exact" && ok:
			if !(config.Policy{Mode: config.ModeRepo, Repositories: []string{c.Entry}}).Admits(config.Repository{Origin: e.key}) {
				t.Errorf("Admits refuses %q, which the server stores", c.Entry)
			}
		case c.Server == "wildcard" && ok:
			if (config.Policy{Mode: config.ModeRepo, Repositories: []string{c.Entry}}).Admits(config.Repository{Origin: e.key + "/any"}) {
				t.Errorf("Admits takes %q as a wildcard without one", c.Entry)
			}
		}
	}
}

// reading is how ParseEntry reads an entry: exact, wildcard or refused.
func reading(entry string, wildcards bool) string {
	e, ok := ParseEntry(entry, wildcards)
	switch {
	case !ok:
		return "refused"
	case e.Wildcard():
		return "wildcard"
	default:
		return "exact"
	}
}
