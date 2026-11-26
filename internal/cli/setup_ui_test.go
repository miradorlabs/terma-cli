package cli

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// flat is s with every run of whitespace one space, for matching wrapped output.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

func TestWrapStep(t *testing.T) {
	for _, tc := range []struct {
		name, step string
		width      int
		want       []string
	}{
		{"wraps at width", "one two three four", 9, []string{"one two", "three", "four"}},
		{"keeps a command whole", "then run `terma setup --org acme` now", 12, []string{"then run", "`terma setup --org acme`", "now"}},
		{"keeps line breaks", "first line\nsecond", 80, []string{"first line", "second"}},
	} {
		if got := wrapStep(tc.step, tc.width); !slices.Equal(got, tc.want) {
			t.Errorf("%s: wrapStep(%q, %d) = %q, want %q", tc.name, tc.step, tc.width, got, tc.want)
		}
	}
}

// Two overlapping teams that share a label are counted as two teams, by enrollment.
func TestConflictLineNamesTeamsSharingALabel(t *testing.T) {
	team := func(id, org string) routing.Team {
		return routing.Team{EnrollmentID: id, OrganizationID: org, TeamID: id,
			Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/acme/api"}}}
	}
	names := teamNames{Orgs: map[string]string{"acme": "Acme"}, Teams: map[string]string{"t1": "Engineering", "t2": "Engineering"}}
	st := routing.State{Teams: []routing.Team{team("t1", "acme"), team("t2", "acme")}}
	if got, want := conflictLine(st, names), "Two teams named 'Acme › Engineering' list the same repository."; got != want {
		t.Fatalf("conflictLine = %q, want %q", got, want)
	}
}

// Output nobody watches is never wrapped, so a copied line stays whole.
func TestSetupUIDoesNotWrapAPipe(t *testing.T) {
	var buf bytes.Buffer
	u := newSetupUI(&buf, false)
	long := strings.Repeat("word ", 30)
	u.Caution("Conflict", long)
	u.Then(long)
	u.finish()
	if strings.Count(buf.String(), strings.TrimSpace(long)) != 2 {
		t.Fatalf("wrapped a pipe's output:\n%s", buf.String())
	}
}
