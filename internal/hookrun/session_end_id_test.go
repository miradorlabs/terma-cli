package hookrun

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Every other handler refuses a session id that is not safe to become a file name and
// a query value. The three session-end handlers spooled whatever they were handed: the
// id reaches the platform as the event's session, and nothing had looked at it.
func TestSessionEndIgnoresAnUnsafeSessionID(t *testing.T) {
	root := initRepo(t)
	for _, tc := range []struct {
		name string
		end  func(context.Context, Env) error
	}{
		{"claude", SessionEnd},
		{"codex", CodexSessionEnd},
		{"opencode", OpenCodeSessionEnd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp, err := spool.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
			t.Setenv("CODEX_HOME", t.TempDir())
			for _, id := range []string{"../../etc/passwd", "two\nlines", strings.Repeat("a", 4096)} {
				stdin := `{"session_id":` + quoteJSON(id) + `,"cwd":` + quoteJSON(root) + `,"reason":"exit"}`
				env := Env{Now: time.Now(), Cwd: root, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
				if err := tc.end(context.Background(), env); err != nil {
					t.Fatalf("%q: a hook must never fail: %v", id, err)
				}
			}
			if events := spooled(t, sp); len(events) != 0 {
				t.Fatalf("spooled %d event(s) for unsafe ids: %+v", len(events), events)
			}
		})
	}
}

func quoteJSON(v string) string {
	b, _ := json.Marshal(v)
	return string(b)
}
