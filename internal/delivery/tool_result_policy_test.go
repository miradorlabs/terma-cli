package delivery

import (
	"fmt"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

func TestToolResultFollowsToolContentPolicy(t *testing.T) {
	for _, flags := range [][2]bool{{false, false}, {false, true}, {true, false}, {true, true}} {
		t.Run(fmt.Sprint(flags), func(t *testing.T) {
			e := spool.Event{Repository: app, Name: semconv.TermaToolCallEvent, Attrs: map[string]any{semconv.GenAIToolCallResultKey: "tool error", semconv.TermaOperationStatusKey: "error"}}
			p := config.Policy{Mode: config.ModeRepo, Repositories: listed, IncludePrompts: flags[0], IncludeToolContent: flags[1]}
			out, ok := (Router{ConfigDir: t.TempDir()}).Outgoing(p, "p1", e)
			if !ok || out.Attrs[semconv.TermaOperationStatusKey] != "error" {
				t.Fatal("failure dropped", out)
			}
			if _, got := out.Attrs[semconv.GenAIToolCallResultKey]; got != flags[1] {
				t.Fatal("tool content policy not applied", out)
			}
			if e.Attrs[semconv.GenAIToolCallResultKey] != "tool error" {
				t.Fatal("spooled content changed")
			}
		})
	}
}
