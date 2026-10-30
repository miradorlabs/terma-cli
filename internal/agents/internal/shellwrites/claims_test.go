package shellwrites

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Two parallel calls claim one file before either records it: one writes it, the other is
// declined. The declined call's unchanged record must not withdraw the other's claim.
func TestParallelCallsKeepAWrittenClaim(t *testing.T) {
	root := hookruntest.InitRepo(t)
	sp, _ := spool.Open(t.TempDir())
	env := hookrun.Env{StateDir: t.TempDir(), Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Spool: sp, Version: "test"}
	r, err := env.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	claims := Claims{Dir: "expected", Tool: "agent"}
	sess := session.Session{ID: "01a08bd5-0487-74b1-9d82-45e619c574fc", Tool: "agent"}
	files := env.Expect(r, sess, []string{filepath.Join(root, "f.txt")})
	claims.expect(env, r, sess, nil, files) // the call that writes, before it runs
	hookruntest.WriteFile(t, root, "f.txt", "agent")
	claims.expect(env, r, sess, nil, files) // the declined call, recorded late
	claims.Settle(env, r, sess.ID)
	if !Held(r, sess.Key())["f.txt"] {
		t.Fatal("the declined call withdrew the parallel call's written claim")
	}
}
