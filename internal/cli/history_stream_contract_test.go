package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/history"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// execWith runs `terma args...` with a custom context and stdout writer.
func execWith(t *testing.T, ctx context.Context, out io.Writer, args ...string) (stderr string, err error) {
	t.Helper()
	defer func() { testApp.flags = globalFlags{} }()
	root := testApp.NewRootCommand()
	root.PersistentPostRun = nil
	var errOut bytes.Buffer
	root.SetOut(out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err = root.ExecuteContext(ctx)
	return errOut.String(), err
}

// checkStreamPlanAccounting checks that every session in plan.sessions reaches exactly one
// terminal outcome (session_done, session_failed, session_skipped, or post-plan team_blocked.skipped).
func checkStreamPlanAccounting(t *testing.T, lines []map[string]any) {
	t.Helper()
	planIdx := -1
	var planned int
	for i, l := range lines {
		if l["type"] == "plan" {
			planIdx = i
			planned = int(l["sessions"].(float64))
			break
		}
	}
	if planIdx < 0 {
		t.Fatalf("no plan line in %s", streamTypes(lines))
	}
	accounted := 0
	for _, l := range lines[planIdx+1:] {
		switch l["type"] {
		case "session_done", "session_failed", "session_skipped":
			accounted++
		case "team_blocked":
			if l["phase"] != "upload" {
				t.Errorf("post-plan team_blocked has phase %q, want %q: %v", l["phase"], "upload", l)
			}
			accounted += int(l["skipped"].(float64))
		}
	}
	if accounted != planned {
		t.Errorf("post-plan outcomes = %d, want plan.sessions = %d (types=%s)", accounted, planned, streamTypes(lines))
	}
}

// D1: Text mode and plain `-o json` behave as on main across refused refresh, sign-out
// between steps, no token at send, discovery-blocked teams, stored earlier failures, and
// resumed+candidate holdings failure.
func TestHistoryUploadD1TextAndJSONMatchMain(t *testing.T) {
	t.Run("refused refresh at holdings", func(t *testing.T) {
		for _, mode := range []string{"json", "text"} {
			t.Run(mode, func(t *testing.T) {
				fx := newHistoryFixture(t)
				cred := storedSession(&fakeAuth{srv: fx.srv})
				cred.ExpiresAt = time.Now().Add(-time.Hour)
				if err := saveSignIn(fx.cfgDir, cred); err != nil {
					t.Fatal(err)
				}
				fx.answer(func(f *historyFixture) {
					f.refreshStatus = http.StatusUnauthorized
					f.refreshBody = `{"error":{"code":"UNAUTHENTICATED","message":"refresh token expired"}}`
				})
				args := []string{"history", "upload"}
				if mode == "json" {
					args = append(args, "-o", "json")
				}
				out, stderr, err := termaRun{}.exec(t, args...)
				if code, ok := exitCodeOf(err); !ok || code != 1 {
					t.Fatalf("exit = %v, want exitWith(1)\nstdout:\n%s\nstderr:\n%s", err, out, stderr)
				}
				if mode == "json" {
					const wantJSON = `{
  "discovered_files": 2,
  "sent_sessions": 0,
  "sent_records": 0,
  "sent_bytes": 0,
  "planned": [
    {
      "enrollment_id": "enr-a",
      "team_id": "team-a",
      "team_label": "team-a",
      "sessions": 1,
      "records": 3,
      "estimated_bytes": 0
    },
    {
      "enrollment_id": "enr-b",
      "team_id": "team-b",
      "team_label": "team-b",
      "sessions": 1,
      "records": 3,
      "estimated_bytes": 0
    }
  ],
  "skipped_sessions": {
    "deselected": 1,
    "holdings_check_failed": 1
  },
  "team_issues": [
    {
      "enrollment_id": "enr-a",
      "team_id": "team-a",
      "team_label": "team-a",
      "skipped": 1,
      "reason": "holdings check failed: not logged in: refresh token expired (UNAUTHENTICATED): session expired, run ` + "`terma setup`" + `"
    },
    {
      "enrollment_id": "enr-b",
      "team_id": "team-b",
      "team_label": "team-b",
      "skipped": 1,
      "reason": "active sign-in account changed during upload"
    }
  ]
}
`
					if out != wantJSON {
						t.Errorf("-o json =\n%s\nwant:\n%s", out, wantJSON)
					}
				} else {
					const wantText = "Sent: 0 session(s), 0 records, 0 B\n" +
						"Skipped sessions: deselected=1, holdings_check_failed=1\n" +
						"Team issues:\n" +
						"  - team-a (1 skipped): holdings check failed: not logged in: refresh token expired (UNAUTHENTICATED): session expired, run `terma setup`\n" +
						"  - team-b (1 skipped): active sign-in account changed during upload\n"
					if out != wantText {
						t.Errorf("text stdout =\n%q\nwant:\n%q", out, wantText)
					}
					if !strings.Contains(stderr, "Checking team-a") || !strings.Contains(stderr, "skipped team-a: holdings check failed:") {
						t.Errorf("text stderr mismatch:\n%s", stderr)
					}
				}
			})
		}
	})

	t.Run("sign-out between steps produces deselected team issue", func(t *testing.T) {
		for _, mode := range []string{"json", "text"} {
			t.Run(mode, func(t *testing.T) {
				fx := newHistoryFixture(t)
				fx.answer(func(f *historyFixture) {
					f.onHoldings = func(proj string) {
						if proj == "team-b" {
							_ = auth.DeleteCredential(fx.cfgDir, config.DefaultProfile)
						}
					}
				})
				args := []string{"history", "upload"}
				if mode == "json" {
					args = append(args, "-o", "json")
				}
				out, stderr, err := termaRun{}.exec(t, args...)
				if code, ok := exitCodeOf(err); !ok || code != 1 {
					t.Fatalf("exit = %v, want exitWith(1)\nstdout:\n%s\nstderr:\n%s", err, out, stderr)
				}
				if mode == "json" {
					const wantJSON = `{
  "discovered_files": 2,
  "sent_sessions": 0,
  "sent_records": 0,
  "sent_bytes": 0,
  "planned": [
    {
      "enrollment_id": "enr-a",
      "team_id": "team-a",
      "team_label": "team-a",
      "sessions": 1,
      "records": 3,
      "estimated_bytes": 0
    },
    {
      "enrollment_id": "enr-b",
      "team_id": "team-b",
      "team_label": "team-b",
      "sessions": 1,
      "records": 3,
      "estimated_bytes": 0
    }
  ],
  "skipped_sessions": {
    "deselected": 2
  },
  "team_issues": [
    {
      "enrollment_id": "enr-a",
      "team_id": "team-a",
      "team_label": "team-a",
      "skipped": 1,
      "reason": "active sign-in account changed during upload"
    },
    {
      "enrollment_id": "enr-b",
      "team_id": "team-b",
      "team_label": "team-b",
      "skipped": 1,
      "reason": "active sign-in account changed during upload"
    }
  ]
}
`
					if out != wantJSON {
						t.Errorf("-o json =\n%s\nwant:\n%s", out, wantJSON)
					}
				} else {
					const wantText = "Sent: 0 session(s), 0 records, 0 B\n" +
						"Skipped sessions: deselected=2\n" +
						"Team issues:\n" +
						"  - team-a (1 skipped): active sign-in account changed during upload\n" +
						"  - team-b (1 skipped): active sign-in account changed during upload\n"
					if out != wantText {
						t.Errorf("text stdout =\n%q\nwant:\n%q", out, wantText)
					}
				}
			})
		}
	})

	t.Run("no token at send produces send_failed team issue", func(t *testing.T) {
		for _, mode := range []string{"json", "text"} {
			t.Run(mode, func(t *testing.T) {
				fx := newHistoryFixture(t)
				if err := os.Remove(fx.cxPath); err != nil {
					t.Fatal(err)
				}
				// Resume state skips holdings so BeforeAttempt passes with the stored identity and
				// attempt 0 reaches Transport.credential -> signIn.Token() -> refresh refused -> ("", nil).
				rs := history.NewRouteState(fx.account, config.DefaultProfile, "enr-a", "team-a")
				rs.MarkInProgress("claude", "11111111-1111-1111-1111-111111111111", time.Now().Add(-time.Hour), 3, 100, time.Time{})
				if err := history.SaveState(fx.stateDir, rs); err != nil {
					t.Fatal(err)
				}
				cred := storedSession(&fakeAuth{srv: fx.srv})
				cred.ExpiresAt = time.Now().Add(-time.Hour)
				if err := saveSignIn(fx.cfgDir, cred); err != nil {
					t.Fatal(err)
				}
				fx.answer(func(f *historyFixture) {
					f.refreshStatus = http.StatusUnauthorized
					f.refreshBody = `{"error":{"code":"UNAUTHENTICATED","message":"refresh revoked"}}`
				})
				args := []string{"history", "upload"}
				if mode == "json" {
					args = append(args, "-o", "json")
				}
				out, stderr, err := termaRun{}.exec(t, args...)
				if code, ok := exitCodeOf(err); !ok || code != 1 {
					t.Fatalf("exit = %v, want exitWith(1)\nstdout:\n%s\nstderr:\n%s", err, out, stderr)
				}
				if mode == "json" {
					const wantJSON = `{
  "discovered_files": 1,
  "sent_sessions": 0,
  "sent_records": 0,
  "sent_bytes": 0,
  "planned": [
    {
      "enrollment_id": "enr-a",
      "team_id": "team-a",
      "team_label": "team-a",
      "sessions": 1,
      "resume_sessions": 1,
      "records": 3,
      "estimated_bytes": 0
    }
  ],
  "skipped_sessions": {
    "send_failed": 1
  },
  "team_issues": [
    {
      "enrollment_id": "enr-a",
      "team_id": "team-a",
      "team_label": "team-a",
      "skipped": 1,
      "reason": "no active sign-in token for this account"
    }
  ]
}
`
					if out != wantJSON {
						t.Errorf("-o json =\n%s\nwant:\n%s", out, wantJSON)
					}
				} else {
					const wantText = "Sent: 0 session(s), 0 records, 0 B\n" +
						"Skipped sessions: send_failed=1\n" +
						"Team issues:\n" +
						"  - team-a (1 skipped): no active sign-in token for this account\n"
					if out != wantText {
						t.Errorf("text stdout =\n%q\nwant:\n%q", out, wantText)
					}
				}
			})
		}
	})

	t.Run("discovery-blocked team matches main golden output with no extra stderr line", func(t *testing.T) {
		fx := newHistoryFixture(t)
		if _, err := routing.Update(fx.stateDir, config.DefaultProfile, routing.Expect{Account: fx.account}, func(s *routing.State) error {
			for i := range s.Teams {
				if s.Teams[i].TeamID == "team-a" {
					s.Teams[i].Refusals = 1
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(fx.cxPath); err != nil {
			t.Fatal(err)
		}

		const wantText = "Sent: 0 session(s), 0 records, 0 B\nSkipped segments: team_blocked:suspended=1\n"
		out, stderr, err := termaRun{}.exec(t, "history", "upload")
		if err != nil {
			t.Fatalf("text upload: %v\nstdout:\n%s\nstderr:\n%s", err, out, stderr)
		}
		if out != wantText {
			t.Errorf("text stdout =\n%q\nwant:\n%q", out, wantText)
		}
		if stderr != "" {
			t.Errorf("text stderr = %q, want empty (no extra skipped line for discovery-blocked team)", stderr)
		}

		const wantJSON = `{
  "discovered_files": 1,
  "sent_sessions": 0,
  "sent_records": 0,
  "sent_bytes": 0,
  "skipped_segments": {
    "team_blocked:suspended": 1
  }
}
`
		jsonOut, jsonStderr, err := termaRun{}.exec(t, "history", "upload", "-o", "json")
		if err != nil {
			t.Fatalf("json upload: %v\n%s", err, jsonOut)
		}
		if jsonOut != wantJSON {
			t.Errorf("-o json =\n%s\nwant:\n%s", jsonOut, wantJSON)
		}
		if jsonStderr != "" {
			t.Errorf("-o json stderr = %q, want empty", jsonStderr)
		}
	})

	t.Run("stored earlier failure golden in text and json", func(t *testing.T) {
		fx := newHistoryFixture(t)
		if err := os.Remove(fx.cxPath); err != nil {
			t.Fatal(err)
		}
		rs := history.NewRouteState(fx.account, config.DefaultProfile, "enr-a", "team-a")
		rs.MarkFailed("claude", "11111111-1111-1111-1111-111111111111", time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC), "HTTP 400: bad payload", 2)
		if err := history.SaveState(fx.stateDir, rs); err != nil {
			t.Fatal(err)
		}

		const wantText = "Sent: 0 session(s), 0 records, 0 B\n" +
			"Skipped sessions: failed_previously=1\n" +
			"Failed sessions (1):\n" +
			"  - team-a claude:11111111-1111-1111-1111-111111111111 [previous run]: HTTP 400: bad payload\n" +
			"Run `terma history upload --retry-failed` to try failed sessions again.\n"
		out, stderr, err := termaRun{}.exec(t, "history", "upload")
		if err != nil {
			t.Fatalf("text upload: %v\n%s", err, out)
		}
		if out != wantText || stderr != "" {
			t.Errorf("text stdout=\n%q\nwant=\n%q\nstderr=%q", out, wantText, stderr)
		}

		const wantJSON = `{
  "discovered_files": 1,
  "sent_sessions": 0,
  "sent_records": 0,
  "sent_bytes": 0,
  "skipped_sessions": {
    "failed_previously": 1
  },
  "failed": [
    {
      "enrollment_id": "enr-a",
      "team_id": "team-a",
      "team_label": "team-a",
      "source_system": "claude",
      "session_id": "11111111-1111-1111-1111-111111111111",
      "reason": "HTTP 400: bad payload",
      "rejected_records": 2,
      "previously": true,
      "failed_at": "2026-11-20T10:00:00Z"
    }
  ]
}
`
		jsonOut, _, err := termaRun{}.exec(t, "history", "upload", "-o", "json")
		if err != nil {
			t.Fatalf("json upload: %v\n%s", err, jsonOut)
		}
		if jsonOut != wantJSON {
			t.Errorf("-o json =\n%s\nwant:\n%s", jsonOut, wantJSON)
		}
	})
}

type hookWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
	fn  func(p []byte) (int, error)
}

func (w *hookWriter) Write(p []byte) (int, error) {
	if w.fn != nil {
		return w.fn(p)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *hookWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// D2: Single writer with one grace period after cancel; process never exits 0 without a
// complete final line.
func TestHistoryUploadJSONStreamD2WriterContract(t *testing.T) {
	t.Run("cancel during write still ends on complete summary when reader drains within grace", func(t *testing.T) {
		fx := newHistoryFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var w hookWriter
		blocked := false
		w.fn = func(p []byte) (int, error) {
			if !blocked && bytes.Contains(p, []byte(`"type":"session_start"`)) {
				blocked = true
				cancel()
				// Reader drains 20ms after cancel, well inside the 250ms grace period.
				time.Sleep(20 * time.Millisecond)
			}
			w.mu.Lock()
			defer w.mu.Unlock()
			return w.buf.Write(p)
		}
		_, err := execWith(t, ctx, &w, "history", "upload", "--json-stream")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled\nstdout:\n%s", err, w.String())
		}
		lines := streamLines(t, w.String())
		last := lines[len(lines)-1]
		if last["type"] != "summary" {
			t.Fatalf("last line = %v, want complete summary (types=%s)", last, streamTypes(lines))
		}
		var sum history.Summary
		if err := json.Unmarshal([]byte(summaryOf(t, w.String())), &sum); err != nil {
			t.Fatal(err)
		}
		if !sum.Interrupted {
			t.Errorf("summary.Interrupted = false, want true")
		}
		// Never hit /v1/logs because cancel happened during session_start write.
		if fx.receivedBy("team-a") != 0 {
			t.Errorf("received team-a = %d, want 0", fx.receivedBy("team-a"))
		}
	})

	t.Run("session whose last request succeeded before cancel emits session_done matching summary", func(t *testing.T) {
		fx := newHistoryFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var w hookWriter
		cancelled := false
		w.fn = func(p []byte) (int, error) {
			// Cancel right as session_done is about to be written (after the HTTP 200 completed)
			// and hold the pipe briefly before draining within the grace period.
			if !cancelled && bytes.Contains(p, []byte(`"type":"session_done"`)) {
				cancelled = true
				cancel()
				time.Sleep(20 * time.Millisecond)
			}
			w.mu.Lock()
			defer w.mu.Unlock()
			return w.buf.Write(p)
		}
		_, err := execWith(t, ctx, &w, "history", "upload", "--json-stream")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled\nstdout:\n%s", err, w.String())
		}
		lines := streamLines(t, w.String())
		var doneCount int
		var doneBytes float64
		for _, l := range lines {
			if l["type"] == "session_done" {
				doneCount++
				doneBytes += l["sent_bytes"].(float64)
			}
		}
		last := lines[len(lines)-1]
		if last["type"] != "summary" {
			t.Fatalf("last line = %v, want summary (types=%s)", last, streamTypes(lines))
		}
		var sum history.Summary
		if err := json.Unmarshal([]byte(summaryOf(t, w.String())), &sum); err != nil {
			t.Fatal(err)
		}
		if sum.SentSessions != 1 || doneCount != 1 || float64(sum.SentBytes) != doneBytes || !sum.Interrupted {
			t.Errorf("reconcile mismatch: doneCount=%d doneBytes=%v sum=%+v (received=%d)", doneCount, doneBytes, sum, fx.receivedBy("team-a"))
		}
	})

	t.Run("reader that never drains after cancel gets non-zero exit", func(t *testing.T) {
		newHistoryFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		unblock := make(chan struct{})
		defer close(unblock)
		var w hookWriter
		w.fn = func(p []byte) (int, error) {
			if bytes.Contains(p, []byte(`"type":"summary"`)) {
				cancel()
				<-unblock
				return 0, io.ErrClosedPipe
			}
			w.mu.Lock()
			defer w.mu.Unlock()
			return w.buf.Write(p)
		}
		_, err := execWith(t, ctx, &w, "history", "upload", "--dry-run", "--json-stream")
		if err == nil {
			t.Fatal("err = nil, want non-zero error when summary write times out after cancel")
		}
	})

	t.Run("short write or write error on summary never exits 0", func(t *testing.T) {
		for _, mode := range []string{"write_error", "short_write"} {
			t.Run(mode, func(t *testing.T) {
				newHistoryFixture(t)
				var w hookWriter
				w.fn = func(p []byte) (int, error) {
					if bytes.Contains(p, []byte(`"type":"summary"`)) {
						if mode == "short_write" {
							return len(p) / 2, nil
						}
						return 0, io.ErrClosedPipe
					}
					w.mu.Lock()
					defer w.mu.Unlock()
					return w.buf.Write(p)
				}
				_, err := execWith(t, context.Background(), &w, "history", "upload", "--dry-run", "--json-stream")
				if err == nil {
					t.Fatalf("%s: err = nil, want non-nil error (must never exit 0 without complete final line)", mode)
				}
			})
		}
	})
}

// D3: Already-retired sign-in is caught up front in stream mode (even with nothing to send),
// and "no token at send" through the CLI distinguishes lost sign-in (signed_out, exit 6)
// from an environment mismatch (send_failed, exit 1).
func TestHistoryUploadJSONStreamD3SignInUpFrontAndNoTokenAtSend(t *testing.T) {
	t.Run("retired identity with nothing to send fails up front in stream mode", func(t *testing.T) {
		fx := newHistoryFixture(t)
		if err := os.Remove(fx.ccPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(fx.cxPath); err != nil {
			t.Fatal(err)
		}
		if err := auth.MarkRetired(fx.cfgDir, config.DefaultProfile, ""); err != nil {
			t.Fatal(err)
		}

		// Stream mode: only error signed_out, exit 6 (no start, no summary).
		out, _, err := termaRun{}.exec(t, "history", "upload", "--json-stream")
		if code, ok := exitCodeOf(err); !ok || code != 6 {
			t.Fatalf("stream exit = %v, want 6\n%s", err, out)
		}
		lines := streamLines(t, out)
		if got := streamTypes(lines); got != "error" {
			t.Fatalf("lines = %q, want %q\n%s", got, "error", out)
		}
		if code, _ := lastError(t, out); code != "signed_out" {
			t.Errorf("error code = %q, want %q", code, "signed_out")
		}

		// Text mode keeps main's behavior (D1): nothing to send succeeds cleanly.
		textOut, _, textErr := termaRun{}.exec(t, "history", "upload")
		if textErr != nil {
			t.Fatalf("text mode err = %v, want nil\n%s", textErr, textOut)
		}
	})

	t.Run("no token at send due to lost sign-in emits summary then signed_out exit 6", func(t *testing.T) {
		fx := newHistoryFixture(t)
		if err := os.Remove(fx.cxPath); err != nil {
			t.Fatal(err)
		}
		// Resume state skips holdings so BeforeAttempt passes and attempt 0 hits
		// Transport.credential -> signIn.Token() -> refresh refused -> ("", nil) -> ClassifyNoToken.
		rs := history.NewRouteState(fx.account, config.DefaultProfile, "enr-a", "team-a")
		rs.MarkInProgress("claude", "11111111-1111-1111-1111-111111111111", time.Now().Add(-time.Hour), 3, 100, time.Time{})
		if err := history.SaveState(fx.stateDir, rs); err != nil {
			t.Fatal(err)
		}
		cred := storedSession(&fakeAuth{srv: fx.srv})
		cred.ExpiresAt = time.Now().Add(-time.Hour)
		if err := saveSignIn(fx.cfgDir, cred); err != nil {
			t.Fatal(err)
		}
		fx.answer(func(f *historyFixture) {
			f.refreshStatus = http.StatusUnauthorized
			f.refreshBody = `{"error":{"code":"UNAUTHENTICATED","message":"refresh revoked"}}`
		})
		out, _, err := termaRun{}.exec(t, "history", "upload", "--json-stream")
		if code, ok := exitCodeOf(err); !ok || code != 6 {
			t.Fatalf("exit = %v, want 6\n%s", err, out)
		}
		lines := streamLines(t, out)
		if got := streamTypes(lines); got != "start discover plan session_start summary error" {
			t.Fatalf("lines = %q\n%s", got, out)
		}
		if code, _ := lastError(t, out); code != "signed_out" {
			t.Errorf("error code = %q, want %q", code, "signed_out")
		}
	})

	t.Run("no token at send due to auth host mismatch is NOT mapped to signed_out", func(t *testing.T) {
		fx := newHistoryFixture(t)
		if err := os.Remove(fx.cxPath); err != nil {
			t.Fatal(err)
		}
		// Credential is for a different auth host; routing state matches its AccountKey, and
		// resume state skips holdings so send reaches signInSource.Token directly.
		otherAuth := "https://other-auth.example.test"
		cred := storedSession(&fakeAuth{srv: fx.srv})
		cred.AuthURL = otherAuth
		if err := saveSignIn(fx.cfgDir, cred); err != nil {
			t.Fatal(err)
		}
		otherAcct := cred.Account()
		if _, err := routing.Replace(fx.stateDir, config.DefaultProfile, nil, otherAcct, func(s *routing.State) error {
			now := time.Now().UTC()
			s.Teams = []routing.Team{{
				EnrollmentID: "enr-a", TeamID: "team-a", OrganizationID: "org-a", AuthURL: fx.srv.URL,
				Policy: config.Policy{
					Mode: config.ModeRepo, Repositories: []string{"github.com/acme/repo-a"},
					IncludePrompts: true, IncludeToolContent: true,
					OrganizationID: "org-a", TeamID: "team-a", AuthURL: fx.srv.URL, FetchedAt: now,
				},
			}}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		rs := history.NewRouteState(otherAcct, config.DefaultProfile, "enr-a", "team-a")
		rs.MarkInProgress("claude", "11111111-1111-1111-1111-111111111111", time.Now().Add(-time.Hour), 3, 100, time.Time{})
		if err := history.SaveState(fx.stateDir, rs); err != nil {
			t.Fatal(err)
		}

		out, _, err := termaRun{}.exec(t, "history", "upload", "--json-stream")
		if code, ok := exitCodeOf(err); !ok || code != 1 {
			t.Fatalf("exit = %v, want 1 (NOT signed_out/6)\n%s", err, out)
		}
		lines := streamLines(t, out)
		if got := streamTypes(lines); got != "start discover plan session_start team_blocked summary" {
			t.Fatalf("lines = %q, want send_failed summary (not error signed_out)\n%s", got, out)
		}
		if b := lines[4]; b["phase"] != "upload" || b["reason_code"] != "send_failed" || b["skipped"] != float64(1) {
			t.Errorf("team_blocked = %v, want upload send_failed skipped=1", b)
		}
	})
}

// D4: Pre-stream errors (--output= with empty value and directory-resolution failure) emit
// an error line in --json-stream mode.
func TestHistoryUploadJSONStreamD4PreStreamErrors(t *testing.T) {
	t.Run("empty --output= is rejected", func(t *testing.T) {
		newHistoryFixture(t)
		out, _, err := termaRun{}.exec(t, "history", "upload", "--json-stream", "--output=")
		if code, ok := exitCodeOf(err); !ok || code != 1 {
			t.Fatalf("exit = %v, want 1\n%s", err, out)
		}
		lines := streamLines(t, out)
		if got := streamTypes(lines); got != "error" {
			t.Fatalf("lines = %q, want %q\n%s", got, "error", out)
		}
		if code, msg := lastError(t, out); code != "failed" || !strings.Contains(msg, "--output") {
			t.Errorf("error = (%q, %q), want failed mentioning --output", code, msg)
		}
	})

	t.Run("directory resolution error emits stream error", func(t *testing.T) {
		useDirs(t, "", "")
		testApp.dirErr = errors.New("resolve config directory: $HOME is not defined")
		defer func() { testApp.dirErr = nil }()

		out, _, err := termaRun{}.exec(t, "history", "upload", "--json-stream")
		if code, ok := exitCodeOf(err); !ok || code != 1 {
			t.Fatalf("exit = %v, want 1\n%s", err, out)
		}
		lines := streamLines(t, out)
		if got := streamTypes(lines); got != "error" {
			t.Fatalf("lines = %q, want %q\n%s", got, "error", out)
		}
		if code, msg := lastError(t, out); code != "failed" || !strings.Contains(msg, "$HOME is not defined") {
			t.Errorf("error = (%q, %q), want failed with dirErr message", code, msg)
		}

		// Text mode still fails with the uncoded dirErr.
		_, _, textErr := termaRun{}.exec(t, "history", "upload")
		if textErr == nil || !strings.Contains(textErr.Error(), "$HOME is not defined") {
			t.Errorf("text mode err = %v, want dirErr", textErr)
		}
	})
}

// D6 + Codex uploader.go:349: An older resumable session outside --since on a
// discovery-blocked team emits team_blocked (phase "discovery") and accounts for it in
// summary.skipped_segments, then resumes and uploads once the team is unblocked.
func TestHistoryUploadJSONStreamOutsideSinceResumableBlocked(t *testing.T) {
	fx := newHistoryFixture(t)
	if err := os.Remove(fx.cxPath); err != nil {
		t.Fatal(err)
	}
	// Age team-a's transcript beyond --since=24h, and mark it in_progress in resume state.
	veryOld := time.Now().UTC().Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(fx.ccPath, veryOld, veryOld); err != nil {
		t.Fatal(err)
	}
	rs := history.NewRouteState(fx.account, config.DefaultProfile, "enr-a", "team-a")
	rs.MarkInProgress("claude", "11111111-1111-1111-1111-111111111111", time.Now().Add(-time.Hour), 3, 100, time.Time{})
	if err := history.SaveState(fx.stateDir, rs); err != nil {
		t.Fatal(err)
	}
	if _, err := routing.Update(fx.stateDir, config.DefaultProfile, routing.Expect{Account: fx.account}, func(s *routing.State) error {
		for i := range s.Teams {
			if s.Teams[i].TeamID == "team-a" {
				s.Teams[i].Refusals = 1
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	out, _, err := termaRun{}.exec(t, "history", "upload", "--since", "24h", "--json-stream")
	if err != nil {
		t.Fatalf("upload: %v\n%s", err, out)
	}
	lines := streamLines(t, out)
	if got := streamTypes(lines); got != "start discover team_blocked plan summary" {
		t.Fatalf("lines = %q, want start discover team_blocked plan summary\n%s", got, out)
	}
	b := lines[2]
	if b["enrollment_id"] != "enr-a" || b["phase"] != "discovery" ||
		b["reason_code"] != "team_blocked:suspended" || b["skipped"] != float64(1) {
		t.Errorf("team_blocked = %v, want enr-a discovery team_blocked:suspended skipped=1", b)
	}
	var sum history.Summary
	if err := json.Unmarshal([]byte(summaryOf(t, out)), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.SkippedSegments["team_blocked:suspended"] != 1 || len(sum.TeamIssues) != 0 {
		t.Errorf("summary = %+v, want skipped_segments[team_blocked:suspended]=1 and empty team_issues", sum)
	}

	// Unblock team-a: the second run resumes and uploads the older session.
	if _, err := routing.Update(fx.stateDir, config.DefaultProfile, routing.Expect{Account: fx.account}, func(s *routing.State) error {
		for i := range s.Teams {
			s.Teams[i].Refusals = 0
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out2, _, err := termaRun{}.exec(t, "history", "upload", "--since", "24h", "--json-stream")
	if err != nil {
		t.Fatalf("recovery upload: %v\n%s", err, out2)
	}
	if fx.receivedBy("team-a") != 1 {
		t.Errorf("recovery received team-a = %d, want 1", fx.receivedBy("team-a"))
	}
}

// Cancelling a real context during discovery ends on summary (interrupted: true) without
// emitting discover or plan.
func TestHistoryUploadJSONStreamCancelDuringDiscovery(t *testing.T) {
	newHistoryFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var w hookWriter
	w.fn = func(p []byte) (int, error) {
		w.mu.Lock()
		n, err := w.buf.Write(p)
		w.mu.Unlock()
		if bytes.Contains(p, []byte(`"type":"start"`)) {
			// Cancel immediately once start is written so discovery sees a live cancelled context.
			cancel()
		}
		return n, err
	}
	_, err := execWith(t, ctx, &w, "history", "upload", "--json-stream")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled\nstdout:\n%s", err, w.String())
	}
	lines := streamLines(t, w.String())
	if got := streamTypes(lines); got != "start summary" {
		t.Fatalf("lines = %q, want %q\n%s", got, "start summary", w.String())
	}
	var sum history.Summary
	if err := json.Unmarshal([]byte(summaryOf(t, w.String())), &sum); err != nil {
		t.Fatal(err)
	}
	if !sum.Interrupted {
		t.Errorf("summary.Interrupted = false, want true")
	}
}

// Mid-upload team stops (send_failed, org_access_lost on send, and 5 consecutive
// rejections) all emit team_blocked with phase "upload" and reconcile with plan.sessions.
func TestHistoryUploadJSONStreamMidUploadTeamStops(t *testing.T) {
	t.Run("send_failed stops team mid-upload", func(t *testing.T) {
		fx := newHistoryFixture(t)
		fx.answer(func(f *historyFixture) {
			f.logsStatus = map[string]int{"team-a": http.StatusForbidden}
			f.logsBody = map[string]string{"team-a": `{"message":"forbidden"}`}
		})
		out, _, err := termaRun{}.exec(t, "history", "upload", "--json-stream")
		if code, ok := exitCodeOf(err); !ok || code != 1 {
			t.Fatalf("exit = %v, want 1\n%s", err, out)
		}
		lines := streamLines(t, out)
		const want = "start discover holdings holdings plan session_start team_blocked session_start session_done summary"
		if got := streamTypes(lines); got != want {
			t.Fatalf("lines = %q, want %q\n%s", got, want, out)
		}
		if b := lines[6]; b["team_id"] != "team-a" || b["phase"] != "upload" || b["reason_code"] != "send_failed" || b["skipped"] != float64(1) {
			t.Errorf("team_blocked = %v, want team-a upload send_failed skipped=1", b)
		}
		checkStreamPlanAccounting(t, lines)
	})

	t.Run("org_access_lost on send stops both teams and second run recovers", func(t *testing.T) {
		fx := newHistoryFixture(t)
		if _, err := routing.Update(fx.stateDir, config.DefaultProfile, routing.Expect{Account: fx.account}, func(s *routing.State) error {
			for i := range s.Teams {
				s.Teams[i].OrganizationID = "org-shared"
				s.Teams[i].Policy.OrganizationID = "org-shared"
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		fx.answer(func(f *historyFixture) {
			f.logsStatus = map[string]int{"team-a": http.StatusForbidden}
			f.logsBody = map[string]string{"team-a": `{"message":"org blocked","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"ORG_NOT_ACCESSIBLE"}]}`}
		})
		out, _, err := termaRun{}.exec(t, "history", "upload", "--json-stream")
		if code, ok := exitCodeOf(err); !ok || code != 1 {
			t.Fatalf("exit = %v, want 1\n%s", err, out)
		}
		lines := streamLines(t, out)
		const want = "start discover holdings holdings plan session_start team_blocked team_blocked summary"
		if got := streamTypes(lines); got != want {
			t.Fatalf("lines = %q, want %q\n%s", got, want, out)
		}
		for i, wantTeam := range []string{"team-a", "team-b"} {
			b := lines[6+i]
			if b["team_id"] != wantTeam || b["phase"] != "upload" || b["reason_code"] != "org_access_lost" || b["skipped"] != float64(1) {
				t.Errorf("team_blocked[%d] = %v, want %s upload org_access_lost skipped=1", i, b, wantTeam)
			}
		}
		checkStreamPlanAccounting(t, lines)

		// Restore org access and verify a second run uploads both teams' sessions.
		if _, err := routing.Update(fx.stateDir, config.DefaultProfile, routing.Expect{Account: fx.account}, func(s *routing.State) error {
			for i := range s.Teams {
				s.Teams[i].Refusals = 0
				s.Teams[i].RefusedAt = time.Time{}
				s.Teams[i].LastError = ""
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		fx.answer(func(f *historyFixture) {
			f.logsStatus = nil
			f.logsBody = nil
		})
		out2, _, err := termaRun{}.exec(t, "history", "upload", "--json-stream")
		if err != nil {
			t.Fatalf("recovery upload: %v\n%s", err, out2)
		}
		if fx.receivedBy("team-a") != 1 || fx.receivedBy("team-b") != 1 {
			t.Errorf("recovery received team-a=%d team-b=%d, want 1 each", fx.receivedBy("team-a"), fx.receivedBy("team-b"))
		}
	})

	t.Run("five rejections in a row blocks remaining sessions", func(t *testing.T) {
		fx := newHistoryFixture(t)
		if err := os.Remove(fx.cxPath); err != nil {
			t.Fatal(err)
		}
		oldTime := time.Now().UTC().Add(-4 * time.Hour)
		projectDir := filepath.Dir(fx.ccPath)
		// Add 5 more Claude sessions (total 6 for team-a).
		for i := 2; i <= 6; i++ {
			sid := fmt.Sprintf("11111111-1111-1111-1111-11111111111%d", i)
			p := filepath.Join(projectDir, sid+".jsonl")
			content := strings.Join([]string{
				`{"type":"user","uuid":"u1","promptId":"p1","sessionId":"` + sid + `","cwd":` + jsonQuote(fx.repoA) + `,"version":"2.1.301","timestamp":"` + oldTime.Format(time.RFC3339Nano) + `","message":{"role":"user","content":"prompt"}}`,
				`{"type":"assistant","uuid":"a1","sessionId":"` + sid + `","cwd":` + jsonQuote(fx.repoA) + `,"version":"2.1.301","timestamp":"` + oldTime.Add(time.Second).Format(time.RFC3339Nano) + `","requestId":"req_1","message":{"id":"msg_1","role":"assistant","model":"claude-opus-5-5","stop_reason":"end_turn","content":[{"type":"text","text":"reply"}],"usage":{"input_tokens":10,"output_tokens":5}}}`,
			}, "\n") + "\n"
			if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			_ = os.Chtimes(p, oldTime, oldTime)
		}
		fx.answer(func(f *historyFixture) {
			f.logsStatus = map[string]int{"team-a": http.StatusBadRequest}
		})
		out, _, err := termaRun{}.exec(t, "history", "upload", "--json-stream")
		if code, ok := exitCodeOf(err); !ok || code != 1 {
			t.Fatalf("exit = %v, want 1\n%s", err, out)
		}
		lines := streamLines(t, out)
		const want = "start discover holdings plan " +
			"session_start session_failed session_start session_failed " +
			"session_start session_failed session_start session_failed " +
			"session_start session_failed team_blocked summary"
		if got := streamTypes(lines); got != want {
			t.Fatalf("lines = %q\nwant    %q\n%s", got, want, out)
		}
		b := lineOf(t, lines, "team_blocked")
		if b["team_id"] != "team-a" || b["phase"] != "upload" || b["reason_code"] != "send_failed" || b["skipped"] != float64(1) {
			t.Errorf("team_blocked = %v, want team-a upload send_failed skipped=1", b)
		}
		checkStreamPlanAccounting(t, lines)
	})
}
