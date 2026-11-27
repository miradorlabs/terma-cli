package cli

import (
	"bytes"
	"encoding/json"
	"maps"
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

var (
	sizeFields = regexp.MustCompile(`"(estimated_bytes|sent_bytes)": [1-9][0-9]*`)
	timeFields = regexp.MustCompile(`"failed_at": "[^"]+"`)
)

// -o json without --json-stream prints what it printed before --json-stream existed. The
// sizes and failure timestamps depend on the run, so only those values are normalized.
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

	// Failure ordering compatibility: when team A gets a new permanent rejection while team B
	// already holds a previously failed session, team A's failure precedes team B's in both
	// -o json and text mode (matching main's route-order execution).
	t.Run("failure order matches main", func(t *testing.T) {
		fx := newHistoryFixture(t)
		// Run 1: team-a holdings temporarily fails so only team-b uploads and permanently fails.
		fx.answer(func(f *historyFixture) {
			f.holdingsStatus = map[string]int{"team-a": http.StatusServiceUnavailable}
			f.logsStatus = map[string]int{"team-b": http.StatusBadRequest}
		})
		_, _, err := termaRun{}.exec(t, "history", "upload", "-o", "json")
		if code, ok := exitCodeOf(err); !ok || code != 1 {
			t.Fatalf("seed run exit = %v, want 1", err)
		}

		// Run 2: team-a now reaches upload and permanently fails; team-b is already failed_previously.
		fx.answer(func(f *historyFixture) {
			f.holdingsStatus = nil
			f.logsStatus = map[string]int{"team-a": http.StatusBadRequest}
		})
		out, _, err := termaRun{}.exec(t, "history", "upload", "-o", "json")
		if code, ok := exitCodeOf(err); !ok || code != 1 {
			t.Fatalf("run 2 exit = %v, want 1\n%s", err, out)
		}
		const wantFailed = `{
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
    }
  ],
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
      "reason": "HTTP 400: refused",
      "failed_at": "T"
    },
    {
      "enrollment_id": "enr-b",
      "team_id": "team-b",
      "team_label": "team-b",
      "source_system": "codex",
      "session_id": "22222222-2222-2222-2222-222222222222",
      "reason": "HTTP 400: refused",
      "previously": true,
      "failed_at": "T"
    }
  ]
}
`
		got := timeFields.ReplaceAllString(sizeFields.ReplaceAllString(out, `"$1": N`), `"failed_at": "T"`)
		if got != wantFailed {
			t.Errorf("run 2 -o json:\n%s\nwant:\n%s", got, wantFailed)
		}

		// Text mode summary also lists team-a's failure before team-b's.
		textOut, _, err := termaRun{}.exec(t, "history", "upload")
		if err != nil {
			t.Fatalf("text summary run: %v\n%s", err, textOut)
		}
		idxA := strings.Index(textOut, "claude:11111111-1111-1111-1111-111111111111")
		idxB := strings.Index(textOut, "codex:22222222-2222-2222-2222-222222222222")
		if idxA < 0 || idxB < 0 || idxA >= idxB {
			t.Errorf("text summary failure order wrong (idxA=%d idxB=%d):\n%s", idxA, idxB, textOut)
		}
	})
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
		if done["records"] != float64(3) || done["sent_bytes"].(float64) <= 0 {
			t.Errorf("session_done = %v, want 3 records and the sent_bytes", done)
		}
		sentBytes += done["sent_bytes"].(float64)
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
	if got := streamTypes(lines); got != "start discover plan summary" {
		t.Fatalf("second run lines = %s\n%s", got, out)
	}
	if plan := lineOf(t, lines, "plan"); plan["sessions"] != float64(0) ||
		plan["skipped_sessions"].(map[string]any)[history.SkipAlreadyDone] != float64(2) {
		t.Errorf("second plan = %v, want nothing left and 2 already_done", plan)
	}
}

// A session the server holds is counted on the plan; one it rejects ends in
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

	// The server now holds team-b's session: both are counted on an empty plan, per team.
	fx.answer(func(f *historyFixture) {
		f.holdingsStatus = nil
		f.held = map[string][]map[string]any{"team-b": {{"source_system": "codex", "session_id": "22222222-2222-2222-2222-222222222222"}}}
	})
	out, _, err = termaRun{}.exec(t, "history", "upload", "--json-stream")
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, out)
	}
	lines = streamLines(t, out)
	const want2 = "start discover holdings plan summary"
	if got := streamTypes(lines); got != want2 {
		t.Fatalf("second run lines = %s\nwant              %s\n%s", got, want2, out)
	}
	var plan history.UploadPlan
	raw, _ := json.Marshal(lineOf(t, lines, "plan"))
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	wantSkips := map[string]map[string]int{
		"team-a": {history.SkipFailedPreviously: 1},
		"team-b": {history.SkipAlreadyHeld: 1},
	}
	if plan.Sessions != 0 || len(plan.Teams) != 2 || plan.SkippedSessions[history.SkipFailedPreviously] != 1 || plan.SkippedSessions[history.SkipAlreadyHeld] != 1 {
		t.Fatalf("plan = %+v, want nothing left, one failed_previously and one already_held", plan)
	}
	for _, team := range plan.Teams {
		if !maps.Equal(team.SkippedSessions, wantSkips[team.TeamID]) {
			t.Errorf("%s skipped = %v, want %v", team.TeamID, team.SkippedSessions, wantSkips[team.TeamID])
		}
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
		lines := streamLines(t, out)
		if got := streamTypes(lines); got != "error" {
			t.Fatalf("lines = %q, want exact sequence %q (no start or summary before work begins)\n%s", got, "error", out)
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
		check(t, "not_set_up", 5)
	})
	t.Run("signed out", func(t *testing.T) {
		stateDir := t.TempDir()
		useDirs(t, t.TempDir(), stateDir)
		t.Setenv("TERMA_API_KEY", "")
		account := auth.AccountKey("https://auth.example.test", "u-1")
		if _, err := routing.Replace(stateDir, config.DefaultProfile, nil, account, func(*routing.State) error { return nil }); err != nil {
			t.Fatal(err)
		}
		check(t, "signed_out", 6)
	})
	t.Run("upload running", func(t *testing.T) {
		fx := newHistoryFixture(t)
		unlock, err := history.LockUpload(fx.stateDir, fx.account, config.DefaultProfile)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		check(t, "upload_running", 4)
		if fx.holdingsCalls() != 0 || fx.receivedBy("team-a")+fx.receivedBy("team-b") != 0 {
			t.Errorf("a run that found another running reached the server")
		}
	})
}

// Arguments the command does not take end a --json-stream run with an error line too.
func TestHistoryUploadJSONStreamUsageErrors(t *testing.T) {
	useDirs(t, t.TempDir(), t.TempDir())
	for _, args := range [][]string{
		{"history", "upload", "--json-stream", "--no-such-flag"},
		{"history", "upload", "--json-stream", "--active-window"},
		{"history", "upload", "--json-stream", "extra"},
	} {
		out, _, err := termaRun{}.exec(t, args...)
		if code, ok := exitCodeOf(err); !ok || code != 1 {
			t.Fatalf("%v: exit = %v, want 1\n%s", args, err, out)
		}
		lines := streamLines(t, out)
		if got := streamTypes(lines); got != "error" {
			t.Fatalf("%v: lines = %q, want %q\n%s", args, got, "error", out)
		}
		if code, _ := lastError(t, out); code != "failed" {
			t.Errorf("%v: error code %s, want %q", args, code, "failed")
		}
	}
	_, _, err := termaRun{}.exec(t, "history", "upload", "--no-such-flag")
	if err == nil {
		t.Error("text mode took an unknown flag")
	} else if _, coded := exitCodeOf(err); coded {
		t.Errorf("text mode's flag error ended with an exit code of its own: %v", err)
	}
}

func TestArgsAskForJSONStream(t *testing.T) {
	useDirs(t, t.TempDir(), t.TempDir())
	upload, _, err := testApp.NewRootCommand().Find([]string{"history", "upload"})
	if err != nil {
		t.Fatal(err)
	}
	flags := upload.Flags()
	flags.AddFlagSet(upload.InheritedFlags())
	for _, c := range []struct {
		args []string
		want bool
	}{
		{[]string{"history", "upload", "--no-such-flag", "--json-stream"}, true},
		{[]string{"history", "upload", "--json-stream=true", "--bogus"}, true},
		{[]string{"history", "upload", "--json-stream=false", "--bogus"}, false},
		{[]string{"history", "upload", "--since", "--json-stream", "--bogus"}, false},
		{[]string{"history", "upload", "--bogus", "--", "--json-stream"}, false},
		{[]string{"history", "upload", "-o", "--json-stream", "--bogus"}, false},
		{[]string{"history", "upload", "--profile", "--json-stream", "--bogus"}, false},
		{[]string{"history", "upload", "--dry-run", "--json-stream", "--bogus"}, true},
		{[]string{"history", "upload", "--bogus"}, false},
	} {
		if got := argsAskForJSONStream(flags, c.args); got != c.want {
			t.Errorf("%v = %v, want %v", c.args, got, c.want)
		}
	}
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
	for spec, want := range map[string]time.Duration{
		"0":      0,
		"0s":     0,
		"0d":     0,
		"30s":    30 * time.Second,
		"1000ms": time.Second,
		"1h":     time.Hour,
		" 30m ":  30 * time.Minute,
		"2d":     48 * time.Hour,
	} {
		if got, err := parseHistoryActiveWindow(spec); err != nil || got != want {
			t.Errorf("%q = (%v, %v), want %v", spec, got, err, want)
		}
	}
	for _, bad := range []string{"", "-1h", "-1d", "-500ms", "soon", "1x", "999999999d"} {
		if _, err := parseHistoryActiveWindow(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	for _, subSecond := range []string{"500ms", "1500ms", "1.5s", "1ms", "1ns"} {
		err := func() error { _, e := parseHistoryActiveWindow(subSecond); return e }()
		if err == nil || !strings.Contains(err.Error(), "whole seconds") {
			t.Errorf("%q: err = %v, want whole seconds error", subSecond, err)
		}
	}
}

// Every path where sign-in is lost mid-run ends with summary followed by an error line
// carrying code "signed_out" and exit code 6, while text mode keeps its non-coded error.
func TestHistoryUploadJSONStreamSignInLostMidRun(t *testing.T) {
	t.Run("refresh refused during holdings", func(t *testing.T) {
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
		out, _, err := termaRun{}.exec(t, "history", "upload", "--json-stream")
		if code, ok := exitCodeOf(err); !ok || code != 6 {
			t.Fatalf("exit = %v, want 6\n%s", err, out)
		}
		lines := streamLines(t, out)
		if got := streamTypes(lines); got != "start discover holdings summary error" {
			t.Fatalf("lines = %q, want %q\n%s", got, "start discover holdings summary error", out)
		}
		if code, _ := lastError(t, out); code != "signed_out" {
			t.Errorf("error code = %q, want %q", code, "signed_out")
		}
	})

	t.Run("refresh refused in text mode keeps non-coded error", func(t *testing.T) {
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
		_, _, err := termaRun{}.exec(t, "history", "upload")
		if err == nil {
			t.Fatal("text mode did not fail")
		}
		if _, coded := exitCodeOf(err); coded {
			t.Errorf("text mode ended with coded exit error: %v", err)
		}
	})

	t.Run("signed out between holdings and per-session recheck", func(t *testing.T) {
		fx := newHistoryFixture(t)
		fx.answer(func(f *historyFixture) {
			f.onHoldings = func(proj string) {
				if proj == "team-b" {
					_ = auth.DeleteCredential(fx.cfgDir, config.DefaultProfile)
				}
			}
		})
		out, _, err := termaRun{}.exec(t, "history", "upload", "--json-stream")
		if code, ok := exitCodeOf(err); !ok || code != 6 {
			t.Fatalf("exit = %v, want 6\n%s", err, out)
		}
		lines := streamLines(t, out)
		if got := streamTypes(lines); got != "start discover holdings holdings plan summary error" {
			t.Fatalf("lines = %q, want %q\n%s", got, "start discover holdings holdings plan summary error", out)
		}
		if code, _ := lastError(t, out); code != "signed_out" {
			t.Errorf("error code = %q, want %q", code, "signed_out")
		}
	})

	t.Run("sign-in retired on send", func(t *testing.T) {
		fx := newHistoryFixture(t)
		fx.answer(func(f *historyFixture) {
			f.logsStatus = map[string]int{"team-a": http.StatusUnauthorized}
			f.logsBody = map[string]string{"team-a": `{"message":"sign-in retired","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SIGN_IN_RETIRED"}]}`}
		})
		out, _, err := termaRun{}.exec(t, "history", "upload", "--json-stream")
		if code, ok := exitCodeOf(err); !ok || code != 6 {
			t.Fatalf("exit = %v, want 6\n%s", err, out)
		}
		lines := streamLines(t, out)
		if got := streamTypes(lines); got != "start discover holdings holdings plan session_start summary error" {
			t.Fatalf("lines = %q, want %q\n%s", got, "start discover holdings holdings plan session_start summary error", out)
		}
		if code, _ := lastError(t, out); code != "signed_out" {
			t.Errorf("error code = %q, want %q", code, "signed_out")
		}
	})
}

// A team already blocked in routing state at discovery emits team_blocked once before plan
// even when all sessions belong to it and plan is empty.
func TestHistoryUploadJSONStreamDiscoveryBlocked(t *testing.T) {
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
	// Remove team-b's session so every discovered session belongs to the blocked team.
	if err := os.Remove(fx.cxPath); err != nil {
		t.Fatal(err)
	}
	out, _, err := termaRun{}.exec(t, "history", "upload", "--json-stream")
	if err != nil {
		t.Fatalf("upload: %v\n%s", err, out)
	}
	lines := streamLines(t, out)
	const want = "start discover team_blocked plan summary"
	if got := streamTypes(lines); got != want {
		t.Fatalf("lines = %q, want %q\n%s", got, want, out)
	}
	b := lines[2]
	if b["team_id"] != "team-a" || b["enrollment_id"] != "enr-a" ||
		b["reason_code"] != "team_blocked:suspended" || b["skipped"] != float64(1) {
		t.Errorf("team_blocked = %v", b)
	}
	if p := lines[3]; p["sessions"] != float64(0) || p["source_bytes"] != float64(0) {
		t.Errorf("plan = %v, want empty plan", p)
	}
}

// When team A passes holdings and team B in the same organization gets ORG_NOT_ACCESSIBLE,
// both teams are blocked before plan is emitted, so plan counts 0 sessions and 0 source bytes.
func TestHistoryUploadJSONStreamOrgBlockedPlan(t *testing.T) {
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
		f.holdingsStatus = map[string]int{"team-b": http.StatusForbidden}
		f.holdingsBody = map[string]string{"team-b": `{"error":{"code":"PERMISSION_DENIED","reason":"ORG_NOT_ACCESSIBLE","message":"organization is not accessible"}}`}
	})
	out, _, err := termaRun{}.exec(t, "history", "upload", "--json-stream")
	if code, ok := exitCodeOf(err); !ok || code != 1 {
		t.Fatalf("exit = %v, want 1\n%s", err, out)
	}
	lines := streamLines(t, out)
	const want = "start discover holdings holdings team_blocked team_blocked plan summary"
	if got := streamTypes(lines); got != want {
		t.Fatalf("lines = %q, want %q\n%s", got, want, out)
	}
	plan := lineOf(t, lines, "plan")
	if plan["sessions"] != float64(0) || plan["source_bytes"] != float64(0) {
		t.Errorf("plan = %v, want 0 sessions and 0 source_bytes", plan)
	}
	if fx.receivedBy("team-a") != 0 || fx.receivedBy("team-b") != 0 {
		t.Errorf("sent requests for blocked org: team-a=%d team-b=%d", fx.receivedBy("team-a"), fx.receivedBy("team-b"))
	}
}

// A transcript split across two teams counts as 2 session-team uploads in plan and events,
// while plan.source_bytes counts the transcript file once and sent_bytes reconciles with summary.
func TestHistoryUploadJSONStreamSplitTranscript(t *testing.T) {
	fx := newHistoryFixture(t)
	if err := os.Remove(fx.cxPath); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().UTC().Add(-4 * time.Hour)
	splitLines := strings.Join([]string{
		`{"type":"user","uuid":"u1","promptId":"p1","sessionId":"11111111-1111-1111-1111-111111111111","cwd":` + jsonQuote(fx.repoA) + `,"version":"2.1.301","timestamp":"` + oldTime.Format(time.RFC3339Nano) + `","message":{"role":"user","content":"part in repo a"}}`,
		`{"type":"assistant","uuid":"a1","sessionId":"11111111-1111-1111-1111-111111111111","cwd":` + jsonQuote(fx.repoA) + `,"version":"2.1.301","timestamp":"` + oldTime.Add(time.Second).Format(time.RFC3339Nano) + `","requestId":"req_1","message":{"id":"msg_1","role":"assistant","model":"claude-opus-5-5","stop_reason":"end_turn","content":[{"type":"text","text":"reply a"}],"usage":{"input_tokens":10,"output_tokens":5}}}`,
		`{"type":"user","uuid":"u2","promptId":"p2","sessionId":"11111111-1111-1111-1111-111111111111","cwd":` + jsonQuote(fx.repoB) + `,"version":"2.1.301","timestamp":"` + oldTime.Add(2*time.Second).Format(time.RFC3339Nano) + `","message":{"role":"user","content":"part in repo b"}}`,
		`{"type":"assistant","uuid":"a2","sessionId":"11111111-1111-1111-1111-111111111111","cwd":` + jsonQuote(fx.repoB) + `,"version":"2.1.301","timestamp":"` + oldTime.Add(3*time.Second).Format(time.RFC3339Nano) + `","requestId":"req_2","message":{"id":"msg_2","role":"assistant","model":"claude-opus-5-5","stop_reason":"end_turn","content":[{"type":"text","text":"reply b"}],"usage":{"input_tokens":10,"output_tokens":5}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(fx.ccPath, []byte(splitLines), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(fx.ccPath, oldTime, oldTime)
	info, err := os.Stat(fx.ccPath)
	if err != nil {
		t.Fatal(err)
	}

	out, _, err := termaRun{}.exec(t, "history", "upload", "--json-stream")
	if err != nil {
		t.Fatalf("upload: %v\n%s", err, out)
	}
	lines := streamLines(t, out)
	const want = "start discover holdings holdings plan session_start session_done session_start session_done summary"
	if got := streamTypes(lines); got != want {
		t.Fatalf("lines = %q, want %q\n%s", got, want, out)
	}
	plan := lineOf(t, lines, "plan")
	if plan["sessions"] != float64(2) || plan["source_bytes"] != float64(info.Size()) {
		t.Errorf("plan = %v, want 2 sessions and %d source_bytes (file counted once)", plan, info.Size())
	}
	var sentBytes float64
	for i, idx := range []int{6, 8} {
		done := lines[idx]
		if done["index"] != float64(i+1) || done["total"] != float64(2) {
			t.Errorf("session_done[%d] = %v, want %d of 2", i, done, i+1)
		}
		sentBytes += done["sent_bytes"].(float64)
	}
	var sum history.Summary
	if err := json.Unmarshal([]byte(summaryOf(t, out)), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.SentSessions != 2 || float64(sum.SentBytes) != sentBytes {
		t.Errorf("summary = %+v, want 2 sent_sessions and %v sent_bytes", sum, sentBytes)
	}
}
