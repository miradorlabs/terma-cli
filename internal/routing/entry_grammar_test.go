package routing

import (
	"encoding/json"
	"os"
	"testing"
)

// grammarCase is one row of testdata/entry_grammar.json, which the platform's account
// service tests too: whether the server stores the entry, and how ParseEntry reads it.
type grammarCase struct {
	Entry  string `json:"entry"`
	Server bool   `json:"server"`
	CLI    string `json:"cli"`
}

// TestEntryGrammarSharedWithServer reads every entry of the table the account service
// shares: ParseEntry's reading is the table's, and nothing the server stores is refused
// or read as a different kind of entry here.
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
	for _, c := range table.Cases {
		got := "refused"
		if e, ok := ParseEntry(c.Entry, true); ok {
			got = "exact"
			if e.Wildcard() {
				got = "wildcard"
			}
		}
		if got != c.CLI {
			t.Errorf("ParseEntry(%q) reads %s, the table says %s", c.Entry, got, c.CLI)
		}
		if c.Server && c.CLI == "refused" {
			t.Errorf("%q: the server stores it, so the CLI must read it", c.Entry)
		}
		if c.Server && (c.CLI == "wildcard") != serverWildcard(c.Entry) {
			t.Errorf("%q: the server and the CLI disagree on whether it is a wildcard", c.Entry)
		}
	}
}

// serverWildcard is the account service's wildcard test on an entry it stores: a whole
// last `/*` segment.
func serverWildcard(entry string) bool {
	return len(entry) >= 2 && entry[len(entry)-2:] == "/*"
}
