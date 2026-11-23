package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/tailscale/hujson"
)

// agySettings is the shape agy 1.5.13 writes its settings in.
const agySettings = "{\n  \"trustedWorkspaces\": [\n    \"/home/dev/repo\"\n  ]\n}"

func statusLineHome(t *testing.T) (Agent, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Antigravity's status line is not wrapped on Windows")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	return Agent{ConfigDir: t.TempDir()}, filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
}

func writeSettings(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func statusLineOf(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// agy reads it as JSONC.
	std, err := hujson.Standardize(b)
	var doc map[string]any
	if err == nil {
		err = json.Unmarshal(std, &doc)
	}
	if err != nil {
		t.Fatalf("settings no longer JSONC: %v\n%s", err, b)
	}
	sl, _ := doc["statusLine"].(map[string]any)
	return sl
}

func TestAntigravityStatusLineStacksUnderAgysOwnAndTeardownRestoresTheFile(t *testing.T) {
	a, path := statusLineHome(t)
	writeSettings(t, path, agySettings)

	changed, err := a.InstallStatusLine()
	if err != nil || !changed {
		t.Fatalf("install: changed=%v err=%v", changed, err)
	}
	sl := statusLineOf(t, path)
	if sl["type"] != "command" || sl["command"] != antigravityStatusLineCommand("") || sl["stack_with_default"] != true {
		t.Fatalf("statusLine = %v", sl)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), "{\n  \"trustedWorkspaces\": [\n    \"/home/dev/repo\"\n  ],\n  \"statusLine\": ") {
		t.Errorf("the rest of the file moved:\n%s", b)
	}
	st, err := a.StatusLineState("")
	if err != nil || !st.Installed || !st.Stacked || st.Renderer != "" {
		t.Fatalf("state %+v %v", st, err)
	}
	if changed, err := a.InstallStatusLine(); err != nil || changed {
		t.Fatalf("second install: changed=%v err=%v", changed, err)
	}

	restored, err := a.RemoveStatusLine()
	if err != nil || !restored {
		t.Fatalf("remove: %v %v", restored, err)
	}
	if b, _ := os.ReadFile(path); string(b) != agySettings {
		t.Fatalf("not restored byte for byte:\n%q", b)
	}
	if _, err := os.Stat(a.statusLineRecordPath()); !os.IsNotExist(err) {
		t.Errorf("record left behind: %v", err)
	}
}

func TestAntigravityStatusLineWithoutASettingsFileLeavesNone(t *testing.T) {
	a, path := statusLineHome(t)
	if changed, err := a.InstallStatusLine(); err != nil || !changed {
		t.Fatalf("install: %v %v", changed, err)
	}
	if restored, err := a.RemoveStatusLine(); err != nil || !restored {
		t.Fatalf("remove: %v %v", restored, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("settings file left behind: %v", err)
	}
}

// The developer's own status line keeps running behind terma's, its options kept; once agy
// has changed something else in the file, teardown puts back their member alone.
func TestAntigravityStatusLineWrapsTheDevelopersOwn(t *testing.T) {
	a, path := statusLineHome(t)
	own := `{"type": "command", "command": "~/bin/my-line --short", "padding": 1}`
	writeSettings(t, path, "{\n  \"statusLine\": "+own+",\n  \"trustedWorkspaces\": []\n}\n")
	if changed, err := a.InstallStatusLine(); err != nil || !changed {
		t.Fatalf("install: %v %v", changed, err)
	}
	sl := statusLineOf(t, path)
	if sl["command"] != antigravityStatusLineCommand("~/bin/my-line --short") || sl["padding"] != 1.0 || sl["stack_with_default"] != nil {
		t.Fatalf("statusLine = %v", sl)
	}
	if r, err := statusLineRenderer(a.ConfigDir); err != nil || r != "~/bin/my-line --short" {
		t.Fatalf("renderer %q %v", r, err)
	}
	st, _ := a.StatusLineState("")
	if !st.Installed || st.Stacked || st.Renderer != "~/bin/my-line --short" {
		t.Fatalf("state %+v", st)
	}

	// agy trusts a new folder meanwhile.
	b, _ := os.ReadFile(path)
	writeSettings(t, path, strings.Replace(string(b), `"trustedWorkspaces": []`, `"trustedWorkspaces": ["/home/dev/new"]`, 1))
	if restored, err := a.RemoveStatusLine(); err != nil || !restored {
		t.Fatalf("remove: %v %v", restored, err)
	}
	want := "{\n  \"statusLine\": " + own + ",\n  \"trustedWorkspaces\": [\"/home/dev/new\"]\n}\n"
	if b, _ := os.ReadFile(path); string(b) != want {
		t.Fatalf("restored\n%s\nwant\n%s", b, want)
	}
}

// A status line the developer switched off draws nothing of theirs: terma's stacks under
// agy's own, and teardown switches theirs back off.
func TestAntigravityStatusLineSwitchedOff(t *testing.T) {
	a, path := statusLineHome(t)
	own := `{"command":"my-line","enabled":false}`
	writeSettings(t, path, `{"statusLine":`+own+`}`)
	if _, err := a.InstallStatusLine(); err != nil {
		t.Fatal(err)
	}
	sl := statusLineOf(t, path)
	if sl["enabled"] != nil || sl["stack_with_default"] != true || sl["command"] != antigravityStatusLineCommand("") {
		t.Fatalf("statusLine = %v", sl)
	}
	if _, err := a.RemoveStatusLine(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != `{"statusLine":`+own+`}` {
		t.Fatalf("restored %s", b)
	}
}

// A status line the developer replaced after setup is theirs: teardown leaves it, and
// doctor says terma's was replaced.
func TestAntigravityStatusLineReplacedIsLeftAlone(t *testing.T) {
	a, path := statusLineHome(t)
	writeSettings(t, path, agySettings)
	if _, err := a.InstallStatusLine(); err != nil {
		t.Fatal(err)
	}
	writeSettings(t, path, `{"statusLine":{"command":"theirs"}}`)
	if st, _ := a.StatusLineState(""); st.Installed || !st.Replaced {
		t.Fatalf("state %+v", st)
	}
	if restored, err := a.RemoveStatusLine(); err != nil || restored {
		t.Fatalf("remove: %v %v", restored, err)
	}
	if b, _ := os.ReadFile(path); string(b) != `{"statusLine":{"command":"theirs"}}` {
		t.Fatalf("changed %s", b)
	}
}

// olderInstall makes the file what an older terma wrote: its command, and a record of it.
func olderInstall(t *testing.T, a Agent, path string) {
	t.Helper()
	if _, err := a.InstallStatusLine(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	old := strings.Replace(string(b), "|| exit 0", "|| true", 1)
	writeSettings(t, path, old)
	if err := a.updateStatusLineRecords(func(recs map[string]*antigravityStatusLineRecordEntry) { recs[path].After = hashBytes([]byte(old)) }); err != nil {
		t.Fatal(err)
	}
}

func TestAntigravityStatusLineRefreshRewritesAnOlderCommand(t *testing.T) {
	a, path := statusLineHome(t)
	writeSettings(t, path, agySettings)
	olderInstall(t, a, path)
	if _, changed, err := a.RefreshMachine(); err != nil || !changed {
		t.Fatalf("refresh: %v %v", changed, err)
	}
	if sl := statusLineOf(t, path); sl["command"] != antigravityStatusLineCommand("") {
		t.Fatalf("command %v", sl["command"])
	}
	if _, changed, _ := a.RefreshMachine(); changed {
		t.Fatal("refresh is not idempotent")
	}
	if _, err := a.RemoveStatusLine(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != agySettings {
		t.Fatalf("not restored after refresh:\n%s", b)
	}
}

// A refresh after something else changed the file never lets teardown put the old file
// back over that change.
func TestAntigravityStatusLineRefreshKeepsOtherChanges(t *testing.T) {
	a, path := statusLineHome(t)
	writeSettings(t, path, agySettings)
	olderInstall(t, a, path)
	b, _ := os.ReadFile(path)
	writeSettings(t, path, strings.Replace(string(b), `"/home/dev/repo"`, `"/home/dev/repo", "/home/dev/new"`, 1))
	if _, changed, err := a.RefreshMachine(); err != nil || !changed {
		t.Fatalf("refresh: %v %v", changed, err)
	}
	if _, err := a.RemoveStatusLine(); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if !strings.Contains(string(b), "/home/dev/new") || strings.Contains(string(b), "statusLine") {
		t.Fatalf("teardown after refresh:\n%s", b)
	}
}

// Teardown runs for every agent: an agy settings file terma never wrapped, even a broken
// one, is left alone and stops nothing.
func TestAntigravityStatusLineTeardownIgnoresAFileItNeverWrapped(t *testing.T) {
	a, path := statusLineHome(t)
	writeSettings(t, path, `{"statusLine": "not an object"`)
	if restored, err := a.RemoveStatusLine(); err != nil || restored {
		t.Fatalf("remove: %v %v", restored, err)
	}
}

func statusLinePayload(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "statusline_payload.json"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "/home/dev/repo", root)
}

func TestAntigravityStatusLineSpoolsPlanQuotaAndAccountOnChange(t *testing.T) {
	root, stateDir, sp := hookruntest.Project(t)
	payload := statusLinePayload(t, root)
	now := time.Date(2026, 11, 20, 18, 0, 0, 0, time.UTC)
	render := func(stdin, renderer string) (string, int) {
		var out bytes.Buffer
		env := hookrun.Env{StateDir: stateDir, Now: now, Cwd: root, Policy: hookruntest.Admitting(root), Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &out, Spool: sp}
		code := antigravityStatusLine(context.Background(), env, renderer, "")
		return out.String(), code
	}

	out, code := render(payload, `cat >/dev/null; printf 'mine'`)
	if code != 0 || out != "mine" {
		t.Fatalf("renderer: code %d output %q", code, out)
	}
	events := hookruntest.Spooled(t, sp)
	quota, account := hookruntest.Named(events, semconv.TermaSessionQuotaEvent), hookruntest.Named(events, semconv.TermaSessionAccountEvent)
	if len(quota) != 1 || len(account) != 1 {
		t.Fatalf("spooled %s", hookruntest.Names(events))
	}
	q := quota[0]
	seat := shape.SeatID("google", "dev@example.com")
	if q.SessionID != "6db6b4cf-dcf8-445c-baff-a3e4f649d8ce" {
		t.Errorf("quota under %q", q.SessionID)
	}
	for k, want := range map[string]any{
		semconv.GenAIMainAgentNameKey: antigravityTool, semconv.TermaEvidenceSourceKey: sourceAntigravityStatusline,
		semconv.TermaEvidenceStatusKey: hookrun.StatusPresent, semconv.TermaAccountPlanTypeKey: "Antigravity Starter Quota",
		semconv.UserEmailKey: "dev@example.com", semconv.TermaAccountSeatIDKey: seat, semconv.TermaObservationSequenceKey: 1.0,
	} {
		if q.Attrs[k] != want {
			t.Errorf("quota[%q] = %v, want %v", k, q.Attrs[k], want)
		}
	}
	buckets, _ := json.Marshal(q.Attrs[semconv.TermaRateLimitBucketsKey])
	wantBuckets := `[{"id":"3p-weekly","remaining_fraction":0.968278,"resets_at":1795801252},{"id":"gemini-weekly","remaining_fraction":0.8541744,"resets_at":1795797347}]`
	if string(buckets) != wantBuckets {
		t.Errorf("buckets %s\nwant    %s", buckets, wantBuckets)
	}
	for k, want := range map[string]any{
		semconv.TermaAccountPlanTypeKey: "Antigravity Starter Quota", semconv.UserEmailKey: "dev@example.com", semconv.TermaAccountSeatIDKey: seat,
		semconv.TermaEvidenceSourceKey: sourceAntigravityStatusline,
	} {
		if account[0].Attrs[k] != want {
			t.Errorf("account[%q] = %v, want %v", k, account[0].Attrs[k], want)
		}
	}
	// Nothing of the payload's content, paths or display names leaves.
	for _, e := range events {
		for k, v := range e.Attrs {
			if s, ok := v.(string); ok && (strings.Contains(s, root) || strings.Contains(s, "Flash")) {
				t.Errorf("%s carries %s=%q", e.Name, k, s)
			}
		}
	}

	// A redraw that changed nothing spools nothing; a used bucket does.
	if _, code := render(payload, ""); code != 0 {
		t.Fatalf("no renderer: code %d", code)
	}
	if events := hookruntest.Spooled(t, sp); len(events) != 0 {
		t.Fatalf("an unchanged redraw spooled %s", hookruntest.Names(events))
	}
	render(strings.Replace(payload, "0.8541744", "0.85", 1), "")
	if events := hookruntest.Spooled(t, sp); len(hookruntest.Named(events, semconv.TermaSessionQuotaEvent)) != 1 || len(hookruntest.Named(events, semconv.TermaSessionAccountEvent)) != 0 {
		t.Fatalf("a used bucket spooled %s", hookruntest.Names(events))
	}
}

// Before the first prompt agy has no conversation yet: the payload names none, and terma
// has nothing to file the plan under.
func TestAntigravityStatusLineBeforeTheConversationSpoolsNothing(t *testing.T) {
	root, stateDir, sp := hookruntest.Project(t)
	payload := strings.ReplaceAll(statusLinePayload(t, root), "6db6b4cf-dcf8-445c-baff-a3e4f649d8ce", "")
	var out bytes.Buffer
	env := hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Stdin: strings.NewReader(payload), Stdout: &out, Spool: sp}
	if code := antigravityStatusLine(context.Background(), env, "", ""); code != 0 || out.Len() != 0 {
		t.Fatalf("code %d output %q", code, out.String())
	}
	if events := hookruntest.Spooled(t, sp); len(events) != 0 {
		t.Fatalf("spooled %s", hookruntest.Names(events))
	}
}

// With hooks off the renderer still draws, and nothing is captured.
func TestAntigravityStatusLineHooksOffStillRenders(t *testing.T) {
	root, stateDir, _ := hookruntest.Project(t)
	var out bytes.Buffer
	env := hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Stdin: strings.NewReader(statusLinePayload(t, root)), Stdout: &out}
	if code := antigravityStatusLine(context.Background(), env, "cat >/dev/null; echo drawn", ""); code != 0 || out.String() != "drawn\n" {
		t.Fatalf("code %d output %q", code, out.String())
	}
}

// agy draws nothing of a status line set "enabled": false: setup switches terma's back on,
// and teardown still puts back the file as it was before setup.
func TestAntigravityStatusLineSetupSwitchesTermasBackOn(t *testing.T) {
	a, path := statusLineHome(t)
	writeSettings(t, path, agySettings)
	if _, err := a.InstallStatusLine(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	writeSettings(t, path, strings.Replace(string(b), `"type":"command"`, `"enabled":false,"type":"command"`, 1))
	if st, _ := a.StatusLineState(""); st.Installed || !st.Disabled {
		t.Fatalf("a switched-off line: %+v", st)
	}
	if changed, err := a.InstallStatusLine(); err != nil || !changed {
		t.Fatalf("install: %v %v", changed, err)
	}
	if sl := statusLineOf(t, path); sl["enabled"] != nil || sl["command"] != antigravityStatusLineCommand("") {
		t.Fatalf("statusLine = %v", sl)
	}
	if st, _ := a.StatusLineState(""); !st.Installed {
		t.Fatalf("state %+v", st)
	}
	if _, err := a.RemoveStatusLine(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "statusLine") || !strings.Contains(string(b), "/home/dev/repo") {
		t.Fatalf("after teardown:\n%s", b)
	}
}

// A settings file terma cannot read is left alone when it no longer holds terma's
// command, so teardown and setup for another agent go on; one that still does names the
// file in its error.
func TestAntigravityStatusLineUnreadableFileAfterSetup(t *testing.T) {
	a, path := statusLineHome(t)
	writeSettings(t, path, agySettings)
	if _, err := a.InstallStatusLine(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	writeSettings(t, path, string(b)+"}")
	if _, err := a.RemoveStatusLine(); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("a broken file still holding terma's line: %v", err)
	}
	writeSettings(t, path, `{"trustedWorkspaces": [`)
	if restored, err := a.RemoveStatusLine(); err != nil || restored {
		t.Fatalf("a broken file without terma's line: %v %v", restored, err)
	}
	if b, _ := os.ReadFile(path); string(b) != `{"trustedWorkspaces": [` {
		t.Fatalf("changed %s", b)
	}
	if _, err := os.Stat(a.statusLineRecordPath()); !os.IsNotExist(err) {
		t.Errorf("record kept for a wrap that is gone: %v", err)
	}
}

// A settings file teardown cannot read at all keeps the record: the wrap may still be in
// it, with the developer's own status line behind it.
func TestAntigravityStatusLineUnreadableFileKeepsTheRecord(t *testing.T) {
	a, path := statusLineHome(t)
	writeSettings(t, path, agySettings)
	if _, err := a.InstallStatusLine(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("this user reads a file of any mode")
	}
	if _, err := a.RemoveStatusLine(); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("remove: %v", err)
	}
	if rec, err := a.statusLineRecord(path); err != nil || rec == nil {
		t.Fatalf("record dropped: %v %v", rec, err)
	}
}

// agy reads its settings as JSONC: comments and trailing commas stay through setup, and
// teardown puts the file back byte for byte, or keeps them when only the member goes back.
func TestAntigravityStatusLineKeepsJSONC(t *testing.T) {
	a, path := statusLineHome(t)
	jsonc := "{\n  // folders agy trusts\n  \"trustedWorkspaces\": [\n    \"/home/dev/repo\", // mine\n  ],\n}\n"
	writeSettings(t, path, jsonc)
	if changed, err := a.InstallStatusLine(); err != nil || !changed {
		t.Fatalf("install: %v %v", changed, err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "// folders agy trusts") || !strings.Contains(string(b), "// mine") || !strings.Contains(string(b), "terma hook antigravity-statusline") {
		t.Fatalf("after install:\n%s", b)
	}
	if _, err := a.RemoveStatusLine(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != jsonc {
		t.Fatalf("not restored byte for byte:\n%s", b)
	}

	// Changed by agy since setup: only the member goes, the comments stay.
	if _, err := a.InstallStatusLine(); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	writeSettings(t, path, strings.Replace(string(b), `"/home/dev/repo", // mine`, `"/home/dev/repo", // mine
    "/home/dev/new",`, 1))
	if _, err := a.RemoveStatusLine(); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if strings.Contains(string(b), "statusLine") || !strings.Contains(string(b), "// folders agy trusts") || !strings.Contains(string(b), "/home/dev/new") {
		t.Fatalf("after teardown:\n%s", b)
	}
}

// A comment just before the settings' closing brace stays through setup and teardown,
// on one line with what it follows or in an empty object.
func TestAntigravityStatusLineKeepsACommentBeforeTheClosingBrace(t *testing.T) {
	for _, content := range []string{
		`{"other":true /* keep */}`,
		"{\n  \"other\": true // keep\n}\n",
		`{ /* keep */ }`,
	} {
		t.Run(content, func(t *testing.T) {
			a, path := statusLineHome(t)
			writeSettings(t, path, content)
			if _, err := a.InstallStatusLine(); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(path)
			if !strings.Contains(string(b), "keep") || statusLineOf(t, path)["command"] != antigravityStatusLineCommand("") {
				t.Fatalf("after install:\n%s", b)
			}
			// Teardown after another change, so only the member goes back.
			writeSettings(t, path, strings.Replace(string(b), "{", "{\"agy\": 1,", 1))
			if _, err := a.RemoveStatusLine(); err != nil {
				t.Fatal(err)
			}
			if b, _ := os.ReadFile(path); !strings.Contains(string(b), "keep") || strings.Contains(string(b), "statusLine") {
				t.Fatalf("after teardown:\n%s", b)
			}
		})
	}
}
