package agents

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

type plain struct {
	name   string
	events []string
}

func (p plain) Name() string                 { return p.name }
func (p plain) DisplayName() string          { return p.name }
func (plain) Installed(context.Context) bool { return true }
func (p plain) FlushAfter() []string {
	return slices.DeleteFunc(slices.Clone(p.events), func(e string) bool { return !strings.HasSuffix(e, "-stop") })
}
func (p plain) Events() map[string]Handler {
	out := map[string]Handler{}
	for _, e := range p.events {
		out[e] = func(context.Context, hookrun.Env) error { return nil }
	}
	return out
}

// labeled has a capability plain lacks.
type labeled struct{ plain }

func (l labeled) Tool() string { return "tool-" + l.name }

// With and Find see a capability only on the agents that have it, in registry order.
func TestWithAndFindByCapability(t *testing.T) {
	t.Parallel()
	r := New(plain{name: "a"}, labeled{plain{name: "b"}}, labeled{plain{name: "c"}})
	if got := r.With[Labeled](); len(got) != 2 || got[0].Tool() != "tool-b" || got[1].Tool() != "tool-c" {
		t.Fatalf("With[Labeled] = %v", got)
	}
	if _, ok := r.Find[Labeled]("a"); ok {
		t.Fatal("Find gave a capability the agent lacks")
	}
	if l, ok := r.Find[Labeled]("c"); !ok || l.Tool() != "tool-c" {
		t.Fatalf("Find[Labeled](c) = %v, %v", l, ok)
	}
	if a, ok := r.ForTool("tool-b"); !ok || a.Name() != "b" {
		t.Fatalf("ForTool = %v, %v", a, ok)
	}
}

// Each event resolves to its owner, and only the events an agent names flush.
func TestEventsResolveToTheirOwner(t *testing.T) {
	t.Parallel()
	r := New(plain{name: "a", events: []string{"a-stop", "a-edit"}}, plain{name: "b", events: []string{"b-edit"}})
	if e, ok := r.Event("b-edit"); !ok || e.Agent.Name() != "b" || e.Flush {
		t.Fatalf("b-edit = %+v, %v", e, ok)
	}
	if !r.FlushesAfter("a-stop") || r.FlushesAfter("a-edit") {
		t.Fatal("FlushAfter was not honoured")
	}
	if _, ok := r.Event("nobody"); ok {
		t.Fatal("an unknown event resolved")
	}
}

// Two agents claiming one event is a build error, never a merge.
func TestTwoOwnersOfAnEventPanic(t *testing.T) {
	t.Parallel()
	r := New(plain{name: "a", events: []string{"edit"}}, plain{name: "b", events: []string{"edit"}})
	defer func() {
		if recover() == nil {
			t.Fatal("a shared event was accepted")
		}
	}()
	r.Event("edit")
}
