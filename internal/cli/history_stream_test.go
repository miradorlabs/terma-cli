package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/history"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// streamLines parses --json-stream output, failing on any line that is not one JSON object
// with a type.
func streamLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for raw := range strings.SplitSeq(strings.TrimRight(out, "\n"), "\n") {
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("not a JSON line: %q: %v\nstream:\n%s", raw, err, out)
		}
		if _, ok := line["type"].(string); !ok {
			t.Fatalf("line without a type: %q", raw)
		}
		lines = append(lines, line)
	}
	return lines
}

func streamTypes(lines []map[string]any) string {
	types := make([]string, 0, len(lines))
	for _, l := range lines {
		types = append(types, l["type"].(string))
	}
	return strings.Join(types, " ")
}

func lineOf(t *testing.T, lines []map[string]any, typ string) map[string]any {
	t.Helper()
	for _, l := range lines {
		if l["type"] == typ {
			return l
		}
	}
	t.Fatalf("no %s line in %s", typ, streamTypes(lines))
	return nil
}

// summaryOf renders the summary line as -o json renders a summary.
func summaryOf(t *testing.T, out string) string {
	t.Helper()
	raw := strings.TrimRight(out, "\n")
	last := raw[strings.LastIndex(raw, "\n")+1:]
	var line struct {
		Type    string          `json:"type"`
		Summary history.Summary `json:"summary"`
	}
	if err := json.Unmarshal([]byte(last), &line); err != nil || line.Type != "summary" {
		t.Fatalf("last line is not a summary (%v): %s", err, last)
	}
	var b bytes.Buffer
	if err := output.Render(&b, output.FormatJSON, output.Table{}, line.Summary); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

var sizeFields = regexp.MustCompile(`"(estimated_bytes|sent_bytes)": [1-9][0-9]*`)

// -o json without --json-stream prints what it printed before --json-stream existed. The
// sizes depend on the fixtures' temporary paths, so a size other than zero is the one
// thing not compared.
func TestHistoryUploadJSONOutputUnchanged(t *testing.T) {
	newHistoryFixture(t)
	const wantDry = `{
  "dry_run": true,
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
      "estimated_bytes": N
    },
    {
      "enrollment_id": "enr-b",
      "team_id": "team-b",
      "team_label": "team-b",
      "sessions": 1,
      "records": 3,
      "estimated_bytes": N
    }
  ]
}
`
	const wantSent = `{
  "discovered_files": 2,
  "sent_sessions": 2,
  "sent_records": 6,
  "sent_bytes": N,
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
  ]
}
`
	for _, run := range []struct {
		args []string
		want string
	}{
		{[]string{"history", "upload", "--dry-run", "-o", "json"}, wantDry},
		{[]string{"history", "upload", "-o", "json"}, wantSent},
	} {
		out, _, err := termaRun{}.exec(t, run.args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", run.args, err, out)
		}
		if got := sizeFields.ReplaceAllString(out, `"$1": N`); got != run.want {
			t.Errorf("%v printed:\n%s\nwant:\n%s", run.args, got, run.want)
		}
	}
}

// A dry run streams start, discover, the plan before any server is asked, and the summary
// -o json prints, sending nothing and writing no state.
func TestHistoryUploadJSONStreamDryRun(t *testing.T) {
	fx := newHistoryFixture(t)
	want, _, err := termaRun{}.exec(t, "history", "upload", "--dry-run", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := termaRun{}.exec(t, "history", "upload", "--dry-run", "--json-stream")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	lines := streamLines(t, out)
	if got := streamTypes(lines); got != "start discover plan summary" {
		t.Fatalf("lines = %s, want start discover plan summary\n%s", got, out)
	}
	if got := summaryOf(t, out); got != want {
		t.Errorf("summary line:\n%s\nwant what -o json prints:\n%s", got, want)
	}
	var drySum history.Summary
	if err := json.Unmarshal([]byte(want), &drySum); err != nil {
		t.Fatal(err)
	}

	var plan struct {
		history.UploadPlan
		Type string `json:"type"`
	}
	raw, _ := json.Marshal(lineOf(t, lines, "plan"))
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	ccInfo, _ := os.Stat(fx.ccPath)
	cxInfo, _ := os.Stat(fx.cxPath)
	if plan.HoldingsChecked || plan.Sessions != 2 || plan.Records != 6 || len(plan.Teams) != 2 ||
		plan.SourceBytes != ccInfo.Size()+cxInfo.Size() ||
		plan.EstimatedBytes != drySum.Planned[0].EstimatedBytes+drySum.Planned[1].EstimatedBytes {
		t.Errorf("plan = %+v, want 2 sessions, 6 records, %d source bytes, the summary's estimate, holdings not checked", plan.UploadPlan, ccInfo.Size()+cxInfo.Size())
	}

	start := lineOf(t, lines, "start")
	if start["dry_run"] != true || start["active_window_seconds"] != float64(3600) {
		t.Errorf("start = %v, want a dry run with a one-hour window", start)
	}
	withHistory := map[string]bool{}
	for _, a := range start["agents"].([]any) {
		agent := a.(map[string]any)
		withHistory[agent["name"].(string)] = agent["history"].(bool)
	}
	if !withHistory["claude"] || !withHistory["codex"] || len(withHistory) < 3 {
		t.Errorf("agents = %v, want claude and codex with history among the others", withHistory)
	}
	for name, reads := range withHistory {
		if reads && name != "claude" && name != "codex" {
			t.Errorf("agent %s says it has history", name)
		}
	}
	if fx.holdingsCalls() != 0 || fx.receivedBy("team-a")+fx.receivedBy("team-b") != 0 {
		t.Errorf("the dry run reached the server")
	}
	if _, err := os.Stat(filepath.Join(fx.stateDir, history.Dir)); !os.IsNotExist(err) {
		t.Errorf("the dry run wrote history state: %v", err)
	}
}

// An upload streams each team's holdings check, then one plan of what is left in all, then
// each session in plan order, numbered across teams, and the summary last; a second run
// skips both sessions before its empty plan.
func TestHistoryUploadJSONStreamUpload(t *testing.T) {
	fx := newHistoryFixture(t)
	out, _, err := termaRun{}.exec(t, "history", "upload", "--json-stream")
	if err != nil {
		t.Fatalf("upload: %v\n%s", err, out)
	}
	lines := streamLines(t, out)
	const want = "start discover holdings holdings plan session_start session_done session_start session_done summary"
	if got := streamTypes(lines); got != want {
		t.Fatalf("lines = %s\nwant    %s\n%s", got, want, out)
	}
	plan := lineOf(t, lines, "plan")
	ccInfo, _ := os.Stat(fx.ccPath)
	cxInfo, _ := os.Stat(fx.cxPath)
	if plan["holdings_checked"] != true || plan["sessions"] != float64(2) || plan["source_bytes"] != float64(ccInfo.Size()+cxInfo.Size()) {
		t.Errorf("plan = %v, want 2 sessions of %d bytes, holdings checked", plan, ccInfo.Size()+cxInfo.Size())
	}
	if _, ok := plan["estimated_bytes"]; ok {
		t.Errorf("plan = %v: an upload does not estimate bytes", plan)
	}
	var sentBytes float64
	for i, team := range []string{"team-a", "team-b"} {
		start, done := lines[5+2*i], lines[6+2*i]
		for _, l := range []map[string]any{start, done} {
			if l["team_id"] != team || l["index"] != float64(i+1) || l["total"] != float64(2) || l["session_id"] == "" {
				t.Errorf("%s = %v, want %s, session %d of 2", l["type"], l, team, i+1)
			}
		}
		if done["records"] != float64(3) || done["bytes"].(float64) <= 0 {
			t.Errorf("session_done = %v, want 3 records and the bytes sent", done)
		}
		sentBytes += done["bytes"].(float64)
	}
	var sum history.Summary
	if err := json.Unmarshal([]byte(summaryOf(t, out)), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.SentSessions != 2 || float64(sum.SentBytes) != sentBytes {
		t.Errorf("summary = %+v, want 2 sessions and %v bytes, as the session lines say", sum, sentBytes)
	}

	out, _, err = termaRun{}.exec(t, "history", "upload", "--json-stream")
	if err != nil {
		t.Fatalf("second upload: %v\n%s", err, out)
	}
	lines = streamLines(t, out)
	if got := streamTypes(lines); got != "start discover session_skipped session_skipped plan summary" {
		t.Fatalf("second run lines = %s\n%s", got, out)
	}
	if lines[2]["reason"] != history.SkipAlreadyDone || lines[2]["index"] != nil {
		t.Errorf("skipped = %v, want already_done, outside the plan", lines[2])
	}
	if plan := lineOf(t, lines, "plan"); plan["sessions"] != float64(0) {
		t.Errorf("second plan = %v, want nothing left", plan)
	}
}

// A session the server holds is skipped before the plan; one it rejects ends in
// session_failed; a team whose holdings check fails is blocked before the plan, and the run
// ends on its summary with exit code 1.
func TestHistoryUploadJSONStreamHeldFailedAndBlocked(t *testing.T) {
	fx := newHistoryFixture(t)
	fx.answer(func(f *historyFixture) {
		f.holdingsStatus = map[string]int{"team-b": http.StatusServiceUnavailable}
		f.logsStatus = map[string]int{"team-a": http.StatusBadRequest}
	})
	out, _, err := termaRun{}.exec(t, "history", "upload", "--json-stream")
	if code, ok := exitCodeOf(err); !ok || code != 1 {
		t.Fatalf("exit = %v, want 1\n%s", err, out)
	}
	lines := streamLines(t, out)
	const want = "start discover holdings holdings team_blocked plan session_start session_failed summary"
	if got := streamTypes(lines); got != want {
		t.Fatalf("lines = %s\nwant    %s\n%s", got, want, out)
	}
	if b := lines[4]; b["team_id"] != "team-b" || b["reason_code"] != history.SkipHoldingsFailed || b["skipped"] != float64(1) {
		t.Errorf("team_blocked = %v, want team-b's one session, holdings_check_failed", b)
	}
	if p := lines[5]; p["sessions"] != float64(1) {
		t.Errorf("plan = %v, want team-a's one session", p)
	}
	if f := lines[7]; f["team_id"] != "team-a" || f["index"] != float64(1) || f["total"] != float64(1) || f["reason"] == "" {
		t.Errorf("session_failed = %v, want team-a's session 1 of 1 with the server's reason", f)
	}

	// The server now holds team-b's session: skipped as held, before an empty plan.
	fx.answer(func(f *historyFixture) {
		f.holdingsStatus = nil
		f.held = map[string][]map[string]any{"team-b": {{"source_system": "codex", "session_id": "22222222-2222-2222-2222-222222222222"}}}
	})
	out, _, err = termaRun{}.exec(t, "history", "upload", "--json-stream")
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, out)
	}
	lines = streamLines(t, out)
	const want2 = "start discover session_skipped holdings session_skipped plan summary"
	if got := streamTypes(lines); got != want2 {
		t.Fatalf("second run lines = %s\nwant              %s\n%s", got, want2, out)
	}
	if s := lines[2]; s["team_id"] != "team-a" || s["reason"] != history.SkipFailedPreviously {
		t.Errorf("first skip = %v, want team-a's failed_previously", s)
	}
	if s := lines[4]; s["team_id"] != "team-b" || s["reason"] != history.SkipAlreadyHeld {
		t.Errorf("second skip = %v, want team-b's already_held", s)
	}
}

// lastError is the error line a --json-stream run ended with.
func lastError(t *testing.T, out string) (code, message string) {
	t.Helper()
	lines := streamLines(t, out)
	last := lines[len(lines)-1]
	e, ok := last["error"].(map[string]any)
	if last["type"] != "error" || !ok {
		t.Fatalf("last line is not an error: %v", last)
	}
	return e["code"].(string), e["message"].(string)
}

func TestHistoryUploadJSONStreamErrorCodes(t *testing.T) {
	check := func(t *testing.T, wantCode string, wantExit int) {
		t.Helper()
		_, _, textErr := termaRun{}.exec(t, "history", "upload")
		if textErr == nil {
			t.Fatal("text mode did not fail")
		}
		if _, coded := exitCodeOf(textErr); coded {
			t.Errorf("text mode ended with an exit code of its own: %v", textErr)
		}
		out, stderr, err := termaRun{}.exec(t, "history", "upload", "--json-stream")
		if code, ok := exitCodeOf(err); !ok || code != wantExit {
			t.Fatalf("exit = %v, want %d\n%s", err, wantExit, out)
		}
		code, message := lastError(t, out)
		if code != wantCode || message != textErr.Error() {
			t.Errorf("error = %s %q, want %s and text mode's message %q", code, message, wantCode, textErr.Error())
		}
		if !strings.Contains(stderr, wantCode) {
			t.Errorf("stderr does not name %s: %q", wantCode, stderr)
		}
	}

	t.Run("not set up", func(t *testing.T) {
		useDirs(t, t.TempDir(), t.TempDir())
		t.Setenv("TERMA_API_KEY", "")
		check(t, historyErrNotSetUp, ExitNotSetUp)
	})
	t.Run("signed out", func(t *testing.T) {
		stateDir := t.TempDir()
		useDirs(t, t.TempDir(), stateDir)
		t.Setenv("TERMA_API_KEY", "")
		account := auth.AccountKey("https://auth.example.test", "u-1")
		if _, err := routing.Replace(stateDir, config.DefaultProfile, nil, account, func(*routing.State) error { return nil }); err != nil {
			t.Fatal(err)
		}
		check(t, historyErrSignedOut, ExitSignedOut)
	})
	t.Run("upload running", func(t *testing.T) {
		fx := newHistoryFixture(t)
		unlock, err := history.LockUpload(fx.stateDir, fx.account, config.DefaultProfile)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		check(t, historyErrUploadRunning, ExitUploadRunning)
		if fx.holdingsCalls() != 0 || fx.receivedBy("team-a")+fx.receivedBy("team-b") != 0 {
			t.Errorf("a run that found another running reached the server")
		}
	})
}

// --active-window sets how recently a session may have changed for it to be left out as
// still running: one hour by default, none at zero.
func TestHistoryUploadActiveWindow(t *testing.T) {
	fx := newHistoryFixture(t)
	recent := time.Now().Add(-time.Minute)
	if err := os.Chtimes(fx.ccPath, recent, recent); err != nil {
		t.Fatal(err)
	}
	dryRun := func(args ...string) history.Summary {
		t.Helper()
		out, _, err := termaRun{}.exec(t, append([]string{"history", "upload", "--dry-run", "-o", "json"}, args...)...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		var sum history.Summary
		if err := json.Unmarshal([]byte(out), &sum); err != nil {
			t.Fatal(err)
		}
		return sum
	}
	for _, c := range []struct {
		args          []string
		active, plans int
	}{
		{nil, 1, 1},
		{[]string{"--active-window", "1h"}, 1, 1},
		{[]string{"--active-window", "0"}, 0, 2},
		{[]string{"--active-window", "0d"}, 0, 2},
		{[]string{"--active-window", "30s"}, 0, 2},
		{[]string{"--active-window", "5h"}, 2, 0},
	} {
		sum := dryRun(c.args...)
		if sum.SkippedSessions[history.SkipActive] != c.active || len(sum.Planned) != c.plans {
			t.Errorf("%v: active=%d planned=%d, want %d and %d", c.args, sum.SkippedSessions[history.SkipActive], len(sum.Planned), c.active, c.plans)
		}
	}

	out, _, err := termaRun{}.exec(t, "history", "upload", "--active-window", "0", "--json-stream")
	if err != nil {
		t.Fatalf("upload: %v\n%s", err, out)
	}
	if start := lineOf(t, streamLines(t, out), "start"); start["active_window_seconds"] != float64(0) {
		t.Errorf("start = %v, want a zero window", start)
	}
	if fx.receivedBy("team-a") != 1 || fx.receivedBy("team-b") != 1 {
		t.Errorf("with no window, sent team-a=%d team-b=%d, want both sessions", fx.receivedBy("team-a"), fx.receivedBy("team-b"))
	}
}

func TestParseHistoryActiveWindow(t *testing.T) {
	t.Parallel()
	for spec, want := range map[string]time.Duration{"0": 0, "0d": 0, "1h": time.Hour, " 30m ": 30 * time.Minute, "2d": 48 * time.Hour} {
		if got, err := parseHistoryActiveWindow(spec); err != nil || got != want {
			t.Errorf("%q = (%v, %v), want %v", spec, got, err, want)
		}
	}
	for _, bad := range []string{"", "-1h", "-1d", "soon", "1x"} {
		if _, err := parseHistoryActiveWindow(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}
