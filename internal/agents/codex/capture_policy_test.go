package codex

import (
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

func TestPolicyBlocksCodexReplyCapture(t *testing.T) {
	for _, mode := range []string{config.ModeRepo, config.ModeGlobal} {
		for _, rule := range []string{"prompts", "nothing", "paths", "agent"} {
			t.Run(mode+"/"+rule, func(t *testing.T) {
				env := fundingEnv(t)
				env.Policy.Mode = mode
				routeCodex(t, true)
				switch rule {
				case "prompts":
					env.Policy.IncludePrompts = false
				case "nothing":
					env.Policy.CollectsNothing = true
				case "paths":
					env.Policy.ExcludePaths = []string{".env"}
				case "agent":
					rec, _, err := routing.LoadRecord("project-a")
					if err != nil {
						t.Fatal(err)
					}
					rec.Harnesses = []string{"claude"}
					if err := routing.SaveRecord(rec); err != nil {
						t.Fatal(err)
					}
				}
				if replies := stopCodex(t, env, replyRollout(t)); len(replies) != 0 {
					t.Fatalf("%d private replies spooled", len(replies))
				}
			})
		}
	}
}
