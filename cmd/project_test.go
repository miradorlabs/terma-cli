package cmd

import (
	"strings"
	"testing"
)

func testProjects() []project {
	return []project{
		{ID: "11111111-1111-1111-1111-111111111111", Name: "payments"},
		{ID: "22222222-2222-2222-2222-222222222222", Name: "payments-staging"},
		{ID: "33333333-3333-3333-3333-333333333333", Name: "checkout"},
	}
}

func TestMatchProject(t *testing.T) {
	projects := testProjects()

	t.Run("matches an exact id", func(t *testing.T) {
		got, err := matchProject(projects, "33333333-3333-3333-3333-333333333333")
		if err != nil {
			t.Fatalf("matchProject: %v", err)
		}
		if got.Name != "checkout" {
			t.Errorf("matched %q", got.Name)
		}
	})

	t.Run("prefers an exact name over a prefix", func(t *testing.T) {
		// "payments" is also a prefix of "payments-staging"; an exact hit must win
		// rather than being reported as ambiguous.
		got, err := matchProject(projects, "payments")
		if err != nil {
			t.Fatalf("matchProject: %v", err)
		}
		if got.ID != "11111111-1111-1111-1111-111111111111" {
			t.Errorf("matched %q, want the exactly-named project", got.Name)
		}
	})

	t.Run("matches a unique prefix", func(t *testing.T) {
		got, err := matchProject(projects, "check")
		if err != nil {
			t.Fatalf("matchProject: %v", err)
		}
		if got.Name != "checkout" {
			t.Errorf("matched %q", got.Name)
		}
	})

	t.Run("name matching ignores case", func(t *testing.T) {
		got, err := matchProject(projects, "CheckOut")
		if err != nil {
			t.Fatalf("matchProject: %v", err)
		}
		if got.Name != "checkout" {
			t.Errorf("matched %q", got.Name)
		}
	})

	t.Run("rejects an ambiguous prefix rather than guessing", func(t *testing.T) {
		// Silently picking one would point every later read at a project the user
		// did not choose — the worst possible failure for this command.
		_, err := matchProject(projects, "pay")
		if err == nil {
			t.Fatal("expected an ambiguity error")
		}
		for _, want := range []string{"payments", "payments-staging"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should list the candidates, got %v", err)
			}
		}
	})

	t.Run("reports an unknown project", func(t *testing.T) {
		_, err := matchProject(projects, "nope")
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "terma project list") {
			t.Errorf("error should point at the discovery command, got %v", err)
		}
	})
}
