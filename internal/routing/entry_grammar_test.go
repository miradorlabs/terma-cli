package routing

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

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
		admits(t, c)
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

// admits checks c's row through config.Policy.Admits, which decides collection on every
// path today, with one validated policy listing the entry alone. As 2.x reads it, the
// table's reading: an exact entry admits the repository it names, a wildcard every
// repository under its prefix and not the prefix itself, and a refused entry leaves a
// list that admits nothing. As 1.x reads it, an entry with a `*` is a literal repository
// no origin names, and any other reads as under 2.x.
func admits(t *testing.T, c grammarCase) {
	t.Helper()
	for _, wildcards := range []bool{true, false} {
		p := config.Policy{Mode: config.ModeRepo, Repositories: []string{c.Entry}, Wildcards: wildcards,
			TeamID: "team", FetchedAt: time.Now()}
		admitted := func(origin string) bool { return p.Admits(config.Repository{Origin: origin}) }
		e, _ := ParseEntry(c.Entry, wildcards)
		prefix := Normalize(strings.TrimSuffix(strings.TrimSpace(c.Entry), "/*"))
		switch {
		case !wildcards && strings.Contains(c.Entry, "*"):
			if admitted(prefix+"/any") || admitted(prefix) || admitted(Normalize(c.Entry)) {
				t.Errorf("%q: a 1.x policy admits a repository by it", c.Entry)
			}
		case c.CLI == "refused":
			if !p.AdmitsNone() || admitted(Normalize(c.Entry)) {
				t.Errorf("%q (wildcards %v): refused, but Admits takes it", c.Entry, wildcards)
			}
		case c.CLI == "exact":
			if !admitted(e.Key()) || admitted(e.Key()+"/more") {
				t.Errorf("%q (wildcards %v): Admits does not take it as exactly %s", c.Entry, wildcards, e.Key())
			}
		default:
			if !admitted(prefix+"/any") || !admitted(prefix+"/any/deeper") || admitted(prefix) {
				t.Errorf("%q: Admits does not take it as every repository under %s", c.Entry, prefix)
			}
		}
	}
}
