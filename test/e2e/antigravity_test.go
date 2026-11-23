package e2e

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// TestAntigravityRealLogin drives the real Antigravity CLI (`agy`) through the hooks `terma
// setup` writes for it: a conversation of several turns that writes, edits and fails to
// read a file and runs a shell command, a commit of what it wrote, and a turn cancelled
// mid-command. agy has no API-key route the suite can supply, so it runs on the
// developer's own sign-in (TERMA_E2E_AGY_REAL_LOGIN=1) and its own home; setup's hooks,
// written into the sandbox home, are loaded from the scratch repository's
// .agents/hooks.json, which agy reads beside the global one. So the global file's
// location is not exercised here, nor a hook run outside the repository (the unit tests
// cover that). TERMA_E2E_AGY_CAPTURE names a private folder for the delivered OTLP logs
// and the hook payloads.
func TestAntigravityRealLogin(t *testing.T) {
	track(t)
	if !Enabled() {
		t.Skip("live tests run only with TERMA_E2E=1")
	}
	if os.Getenv("TERMA_E2E_AGY_REAL_LOGIN") != "1" {
		Record(t.Name(), "not run", "needs a signed-in agy (TERMA_E2E_AGY_REAL_LOGIN=1)")
		t.Skip("needs a signed-in agy (TERMA_E2E_AGY_REAL_LOGIN=1)")
	}
	binary, err := exec.LookPath(cmp.Or(os.Getenv("TERMA_E2E_AGY_BINARY"), "agy"))
	if err != nil {
		t.Skip("agy is not installed")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	// Every hook in the developer's global file runs in these sessions too: terma's own would
	// record every event twice, against the sandbox's config, and any other would run with
	// tools approved. Only TERMA_E2E_AGY_GLOBAL_HOOKS=1 runs beside hooks that are not terma's.
	if b, err := os.ReadFile(filepath.Join(home, ".gemini", "config", "hooks.json")); err == nil {
		if strings.Contains(string(b), "hook --user antigravity-") {
			t.Skip("this machine's agy already runs terma's hooks; every event would be recorded twice")
		}
		if len(bytes.TrimSpace(b)) > 2 && os.Getenv("TERMA_E2E_AGY_GLOBAL_HOOKS") != "1" {
			t.Skip("~/.gemini/config/hooks.json holds hooks that would run with tools approved; set TERMA_E2E_AGY_GLOBAL_HOOKS=1 to run beside them")
		}
	}
	model := cmp.Or(os.Getenv("TERMA_E2E_AGY_MODEL"), "gemini-3.8-flash-low")
	// agy keeps its sign-in, and the folders it trusts, in the developer's own settings; the
	// trust prompt below adds the scratch repository there. The file is put back byte for
	// byte, after agy has exited (cleanups run last-registered first), and checked.
	agySettings := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	original, originalErr := os.ReadFile(agySettings)
	t.Cleanup(func() {
		if originalErr != nil {
			if !os.IsNotExist(originalErr) {
				t.Errorf("could not read %s before the test: %v", agySettings, originalErr)
				return
			}
			_ = os.Remove(agySettings)
		} else if err := os.WriteFile(agySettings, original, 0o600); err != nil {
			t.Errorf("restore %s: %v", agySettings, err)
		}
		after, err := os.ReadFile(agySettings)
		if (originalErr == nil) != (err == nil) || !bytes.Equal(after, original) {
			t.Errorf("%s is not as it was before the test", agySettings)
		}
	})

	sb := New(t, Isolated)
	sb.SetupAgents = append(sb.SetupAgents, "antigravity")
	sb.Setup(nil, "--team", sb.ProjectID, "--harness", sb.setupList())
	sb.Receiver.Reset()
	written, err := os.ReadFile(filepath.Join(sb.Home, ".gemini", "config", "hooks.json"))
	if err != nil {
		t.Fatalf("terma setup wrote no agy hooks: %v", err)
	}
	if !strings.Contains(string(written), "hook --user antigravity-stop") {
		t.Fatalf("agy hooks lack terma's Stop entry:\n%s", written)
	}
	sb.write(".agents/hooks.json", string(written))
	sb.write(".git/info/exclude", ".agents/\n")
	// Setup wrapped agy's status line in the sandbox home's settings; the interactive run
	// below loads that file with --settings, which agy never saves.
	statusSettings := filepath.Join(sb.Home, ".gemini", "antigravity-cli", "settings.json")
	if b, err := os.ReadFile(statusSettings); err != nil || !strings.Contains(string(b), "terma hook antigravity-statusline") {
		t.Fatalf("terma setup wrote no agy status line: %v\n%s", err, b)
	}

	t.Cleanup(func() {
		dir := os.Getenv("TERMA_E2E_AGY_CAPTURE")
		if dir == "" {
			return
		}
		if err := os.MkdirAll(filepath.Join(dir, "hook-payloads"), 0o700); err != nil {
			t.Error(err)
			return
		}
		merged := &collogspb.ExportLogsServiceRequest{}
		for _, r := range sb.Receiver.Requests() {
			if logs, ok := r.Payload.(*collogspb.ExportLogsServiceRequest); ok && r.DecodeError == nil {
				merged.ResourceLogs = append(merged.ResourceLogs, logs.ResourceLogs...)
			}
		}
		data, err := protojson.MarshalOptions{Indent: " "}.Marshal(merged)
		if err == nil {
			err = os.WriteFile(filepath.Join(dir, "otlp-logs.json"), append(data, '\n'), 0o600)
		}
		if err != nil {
			t.Error(err)
		}
		// The payloads without the prompt, which agy repeats in every hook as lastUserInput.
		files, _ := filepath.Glob(filepath.Join(sb.payloadDir(), "*.json"))
		for _, f := range files {
			var payload map[string]any
			if b, err := os.ReadFile(f); err == nil && json.Unmarshal(b, &payload) == nil {
				delete(payload, "lastUserInput")
				out, _ := json.MarshalIndent(payload, "", " ")
				_ = os.WriteFile(filepath.Join(dir, "hook-payloads", filepath.Base(f)), out, 0o600)
			}
		}
	})

	// agy keeps its sign-in under the developer's home; terma, git and the hooks keep to
	// the sandbox's config, which TERMA_CONFIG_DIR and GIT_CONFIG_GLOBAL name.
	env := append(sb.termaEnv(), "HOME="+home)
	for _, key := range []string{"USER", "LOGNAME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	run := func(prompt, conversation string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
		defer cancel()
		args := []string{"-p", prompt, "--model", model, "--dangerously-skip-permissions", "--output-format", "json"}
		if conversation != "" {
			args = append(args, "--conversation", conversation)
		}
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir, cmd.Env = sb.Repo, env
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		err := cmd.Wait()
		var out struct {
			ConversationID string `json:"conversation_id"`
			Status         string `json:"status"`
			FinishedReason string `json:"finished_reason"`
		}
		_ = json.Unmarshal(stdout.Bytes(), &out)
		t.Logf("agy -p: err=%v status=%q finished=%q conversation=%s", err, out.Status, out.FinishedReason, out.ConversationID)
		if err != nil || out.ConversationID == "" {
			t.Fatalf("agy -p failed: %v\n%s", err, tail(stderr.String(), 2000))
		}
		return cmp.Or(out.ConversationID, conversation)
	}

	conversation := run("Create a file named agy-live.txt containing exactly the text: first turn. Use your file writing tool. "+
		"Then run the shell command `cat agy-live.txt`. Do not commit.", "")
	run("Change agy-live.txt so that it contains exactly the text: second turn. Use your file editing tool, not a shell command. Do not commit.", conversation)
	run("Use your file viewing tool, not a shell command, to read the file missing-agy-file.txt. It does not exist: say so and stop.", conversation)
	if b, err := os.ReadFile(filepath.Join(sb.Repo, "agy-live.txt")); err != nil || strings.TrimSpace(string(b)) != "second turn" {
		t.Fatalf("agy-live.txt = %q, %v", b, err)
	}

	message := sb.Commit("agy live commit")
	if !strings.Contains(message, "Agent-Session-Id: "+conversation) || !strings.Contains(message, "Agent-Tool: antigravity") {
		t.Errorf("commit not stamped with the conversation:\n%s", message)
	}

	// A cancelled turn: agy 1.5.13 runs no hook after it, neither `agy -p` interrupted
	// nor Esc in the interactive CLI, though its hooks guide lists Stop's USER_CANCELED.
	// The turn's hook events wait in the spool for the next turn's flush.
	completed := len(sb.HookPayloads("antigravity-stop"))
	if completed < 3 {
		t.Errorf("want a Stop per completed turn, got %d", completed)
	}
	terminal, err := Start(sb.Repo, env, 40, 140, binary, "--conversation", conversation, "--model", model, "--dangerously-skip-permissions", "--settings", statusSettings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = terminal.Close("/quit", 5*time.Second) })
	// A new folder asks for trust first; a first start then shows a notice that takes the
	// keyboard until Esc dismisses it.
	ready, trust := regexp.MustCompile(`for shortcuts`), regexp.MustCompile(`trust this folder`)
	for {
		i, err := terminal.ExpectAny(30*time.Second, ready, trust)
		if err != nil {
			t.Fatalf("agy did not start: %v\n%s", err, tail(terminal.Text(), 2000))
		}
		if i == 0 {
			break
		}
		terminal.Consume()
		_ = terminal.Send("\r")
	}
	time.Sleep(2 * time.Second)
	if strings.Contains(terminal.Screen(), "esc to dismiss") {
		_ = terminal.Send("\x1b")
		time.Sleep(time.Second)
	}
	// Cancelled while a command runs: the call that asked for it is in the transcript, and
	// no hook of its turn reads it, so it is never sent: the next turn's hooks pass it and
	// count it lost. agy moves a command to the background after five seconds, so the
	// command takes four.
	transcript := filepath.Join(home, ".gemini", "antigravity-cli", "brain", conversation, ".system_generated", "logs", "transcript_full.jsonl")
	prompts, _ := antigravityToolCallPending(transcript, -1)
	if err := terminal.Type("Run the shell command `sleep 4`, then reply with the word done."); err != nil {
		t.Fatal(err)
	}
	var asked time.Time
	cancelled, escaped := "", false
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline) && cancelled == ""; time.Sleep(250 * time.Millisecond) {
		if _, pending := antigravityToolCallPending(transcript, prompts); asked.IsZero() && pending {
			asked = time.Now()
		}
		// While the command runs.
		if !escaped && !asked.IsZero() && time.Since(asked) > time.Second {
			escaped = terminal.Send("\x1b") == nil
		}
		if stops := sb.HookPayloads("antigravity-stop"); len(stops) > completed {
			cancelled, _ = stops[len(stops)-1]["terminationReason"].(string)
		}
		if escaped && time.Since(asked) > 15*time.Second {
			break
		}
	}
	if !escaped || !strings.Contains(terminal.Text(), "Interrupted") {
		t.Fatalf("the turn was not cancelled\n%s", tail(terminal.Text(), 2000))
	}
	switch cancelled {
	case "":
		t.Log("agy ran no Stop hook for the cancelled turn")
	case "USER_CANCELED":
		t.Log("agy now ends a cancelled turn with Stop USER_CANCELED")
	default:
		t.Errorf("a cancelled turn's Stop says %q", cancelled)
	}

	// One more turn: its hooks pass the cancelled turn's call, unsent, and count it lost.
	_ = terminal.Close("/quit", 10*time.Second)
	run("Reply with just the word OK. Use no tools.", conversation)
	checkAntigravityCalls(t, sb, filepath.Join(home, ".gemini", "antigravity-cli", "brain", conversation, ".system_generated", "logs", "transcript_full.jsonl"), conversation)
	checkAntigravitySeat(t, sb, conversation)

	starts := sb.Delivered("terma.session.start", conversation, 30*time.Second)
	if len(starts) == 0 {
		t.Fatal("no terma.session.start delivered")
	}
	var tools, statuses []string
	for _, l := range sb.Receiver.Logs() {
		if l.Attrs["session.id"] != conversation {
			continue
		}
		if l.EventName == "terma.tool.call" {
			tools = append(tools, l.Attrs["gen_ai.tool.name"])
			statuses = append(statuses, l.Attrs["gen_ai.tool.name"]+"="+l.Attrs["terma.operation.status"])
		}
	}
	t.Logf("tool calls: %v", statuses)
	if !slices.Contains(tools, "run_command") {
		t.Errorf("no run_command tool call delivered: %v", tools)
	}
	if !slices.Contains(statuses, "run_command=completed") || !slices.ContainsFunc(statuses, func(s string) bool { return strings.HasSuffix(s, "=error") }) {
		t.Errorf("want a completed command and a failed tool call: %v", statuses)
	}
	touched := sb.Delivered("terma.files.touched", conversation, 10*time.Second)
	if len(touched) == 0 {
		t.Errorf("no terma.files.touched delivered for the written file")
	}
	if len(sb.Delivered("terma.commit.stamped", "", 10*time.Second)) == 0 {
		t.Errorf("no terma.commit.stamped delivered")
	}
}

// checkAntigravityCalls holds every model call agy's transcript recorded to one
// terma.model_call, sent once, with the transcript's counts (input with the cache reads),
// except the cancelled turn's first call: no hook of its turn read it, so it is never sent,
// and the sandbox's spool counts it lost as turn_ended.
func checkAntigravityCalls(t *testing.T, sb *Sandbox, transcript, conversation string) {
	t.Helper()
	data, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatalf("agy's transcript: %v", err)
	}
	type call struct{ model, input, cache, output string }
	want := map[string]call{}
	var prompts, calls []int
	for line := range bytes.Lines(data) {
		var s struct {
			Step  int    `json:"step_index"`
			Type  string `json:"type"`
			Model string `json:"model"`
			In    int    `json:"input_tokens"`
			Cache int    `json:"cache_read_tokens"`
			Out   int    `json:"output_tokens"`
		}
		if json.Unmarshal(line, &s) != nil {
			continue
		}
		switch s.Type {
		case "USER_INPUT":
			prompts = append(prompts, s.Step)
		case "PLANNER_RESPONSE":
			calls = append(calls, s.Step)
			want["step-"+strconv.Itoa(s.Step)] = call{s.Model, strconv.Itoa(s.In + s.Cache), strconv.Itoa(s.Cache), strconv.Itoa(s.Out)}
		}
	}
	// The cancelled turn is the one before the last prompt: its first call follows its prompt.
	cancelledStep := ""
	if len(prompts) >= 2 {
		for _, step := range calls {
			if step > prompts[len(prompts)-2] && step < prompts[len(prompts)-1] {
				cancelledStep = "step-" + strconv.Itoa(step)
				break
			}
		}
	}
	// Every call but the cancelled turn's is expected.
	expected := len(want)
	if cancelledStep != "" {
		expected--
	}
	var got map[string]int
	deadline := time.Now().Add(30 * time.Second)
	for {
		got = map[string]int{}
		for _, l := range sb.Receiver.Logs() {
			if l.EventName != "terma.model_call" || l.Attrs["session.id"] != conversation {
				continue
			}
			id := l.Attrs["terma.model_call.id"]
			got[id]++
			w, ok := want[id]
			if got[id] > 1 || !ok {
				continue
			}
			if l.Attrs["gen_ai.request.model"] != w.model || l.Attrs["gen_ai.usage.input_tokens"] != w.input ||
				l.Attrs["gen_ai.usage.cache_read.input_tokens"] != w.cache || l.Attrs["gen_ai.usage.output_tokens"] != w.output ||
				l.Attrs["terma.usage.status"] != "partial" || l.Attrs["terma.evidence.source"] != "antigravity_transcript" {
				t.Errorf("%s = %v, want %+v", id, l.Attrs, w)
			}
			if _, ok := l.Attrs["gen_ai.usage.reasoning.output_tokens"]; ok {
				t.Errorf("%s carries a reasoning count agy never gave", id)
			}
		}
		if len(got) >= expected || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	for id, n := range got {
		if n != 1 {
			t.Errorf("%s delivered %d times", id, n)
		}
	}
	// A call's turn is the one begun last at or before its step: each turn begins at the
	// initialNumSteps of its PreInvocation at invocation 0.
	starts := antigravityTurnStarts(sb, conversation)
	for _, l := range sb.Receiver.Logs() {
		if l.EventName != "terma.model_call" || l.Attrs["session.id"] != conversation {
			continue
		}
		id := l.Attrs["terma.model_call.id"]
		step, err := strconv.Atoi(strings.TrimPrefix(id, "step-"))
		if err != nil || !strings.HasPrefix(id, "step-") {
			step, err = strconv.Atoi(l.Attrs["terma.step.index"])
		}
		if err != nil {
			t.Errorf("%s names no step (terma.step.index %q)", id, l.Attrs["terma.step.index"])
			continue
		}
		begun := -1
		for _, s := range starts {
			if s <= step && s > begun {
				begun = s
			}
		}
		if begun < 0 {
			t.Errorf("%s is in %q, but no turn began at or before step %d (starts %v)", id, l.Attrs["terma.turn.id"], step, starts)
		} else if want := "turn-" + strconv.Itoa(begun); l.Attrs["terma.turn.id"] != want {
			t.Errorf("%s is in %q, want %s (starts %v)", id, l.Attrs["terma.turn.id"], want, starts)
		}
	}
	for id := range want {
		if got[id] == 0 && id != cancelledStep {
			t.Errorf("%s (in the transcript) was never delivered", id)
		}
	}
	t.Logf("model calls: %d in the transcript, %d delivered; the cancelled turn's first: %q", len(want), len(got), cancelledStep)
	if cancelledStep == "" {
		t.Errorf("the cancelled turn recorded no call")
	} else if got[cancelledStep] != 0 {
		t.Errorf("the cancelled turn's call %s was delivered %d times, want never", cancelledStep, got[cancelledStep])
	}
	if n := antigravityLosses(sb, "turn_ended"); n < 1 {
		t.Errorf("the spool counts %d calls lost as turn_ended, want the cancelled turn's", n)
	}
}

// antigravityTurnStarts is the first step of each turn of the conversation agy's
// PreInvocation hooks began: the initialNumSteps of each payload at invocation 0, a payload
// with no invocationNum included. A payload naming another conversation is not counted.
func antigravityTurnStarts(sb *Sandbox, conversation string) []int {
	var starts []int
	for _, p := range sb.HookPayloads("antigravity-pre-invocation") {
		if id, ok := p["conversationId"].(string); ok && id != conversation {
			continue
		}
		steps, ok := p["initialNumSteps"].(float64)
		if !ok {
			continue
		}
		if n, present := p["invocationNum"]; present {
			if f, ok := n.(float64); !ok || f != 0 {
				continue
			}
		}
		starts = append(starts, int(steps))
	}
	return starts
}

// antigravityLosses is how many events the sandbox's hooks counted lost for reason, from the
// spool's losses: one JSON line per loss, {"reason":…,"count":…}.
func antigravityLosses(sb *Sandbox, reason string) int {
	files, _ := filepath.Glob(filepath.Join(sb.TermaConfig, "spool", "losses", "*.jsonl"))
	n := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for line := range bytes.Lines(data) {
			var l struct {
				Reason string `json:"reason"`
				Count  int    `json:"count"`
			}
			if json.Unmarshal(line, &l) == nil && l.Reason == reason {
				n += l.Count
			}
		}
	}
	return n
}

// checkAntigravitySeat holds the status line to have reported the plan, its quota
// buckets and the account.
func checkAntigravitySeat(t *testing.T, sb *Sandbox, conversation string) {
	t.Helper()
	quota := sb.Delivered("terma.session.quota", conversation, 30*time.Second)
	if len(quota) == 0 {
		t.Fatal("no terma.session.quota from agy's status line")
	}
	q := quota[len(quota)-1].Attrs
	if q["terma.account.plan_type"] == "" || !strings.HasPrefix(q["terma.rate_limit.buckets"], "[{") || q["terma.evidence.source"] != "antigravity_statusline" {
		t.Errorf("quota: %v", q)
	}
	t.Logf("plan %q, buckets %s", q["terma.account.plan_type"], q["terma.rate_limit.buckets"])
	account := sb.Delivered("terma.session.account", conversation, 10*time.Second)
	if len(account) == 0 || account[0].Attrs["terma.account.seat.id"] == "" {
		t.Errorf("account: %v", account)
	}
}

// antigravityToolCallPending counts the transcript's prompts and reports whether a prompt
// after the first `after` of them has a model call that asked for a tool.
func antigravityToolCallPending(transcript string, after int) (prompts int, pending bool) {
	data, err := os.ReadFile(transcript)
	if err != nil {
		return 0, false
	}
	for line := range bytes.Lines(data) {
		var s struct {
			Type      string            `json:"type"`
			ToolCalls []json.RawMessage `json:"tool_calls"`
		}
		if json.Unmarshal(line, &s) != nil {
			continue
		}
		switch {
		case s.Type == "USER_INPUT":
			prompts++
		case s.Type == "PLANNER_RESPONSE" && len(s.ToolCalls) > 0 && after >= 0 && prompts > after:
			pending = true
		}
	}
	return prompts, pending
}
