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
	"strings"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// TestAntigravityRealLogin drives the real Antigravity CLI (`agy`) through the hooks `terma
// setup` writes for it: a conversation of several turns that writes, edits and fails to
// read a file, runs a shell command and uses a repository skill, a commit of what it wrote,
// and a turn cancelled mid-command. agy has no API-key route the suite can supply, so it runs on the
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
	// A repository skill, listed to the model from the conversation's start.
	sb.write(".agents/skills/house-greeting/SKILL.md", "---\nname: house-greeting\ndescription: Greets the person in the house style. Use when asked for a house greeting.\n---\n\nReply with exactly: `GREETING-7741`.\n")

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
	run("Give me a house greeting.", conversation)
	if b, err := os.ReadFile(filepath.Join(sb.Repo, "agy-live.txt")); err != nil || strings.TrimSpace(string(b)) != "second turn" {
		t.Fatalf("agy-live.txt = %q, %v", b, err)
	}

	message := sb.Commit("agy live commit")
	if !strings.Contains(message, "Agent-Session-Id: "+conversation) || !strings.Contains(message, "Agent-Tool: antigravity") {
		t.Errorf("commit not stamped with the conversation:\n%s", message)
	}

	// A cancelled turn: agy 1.5.13 runs no hook after it, neither `agy -p` interrupted
	// nor Esc in the interactive CLI, though its hooks guide lists Stop's USER_CANCELED.
	// The turn's events wait in the spool for the next turn's flush.
	completed := len(sb.HookPayloads("antigravity-stop"))
	if completed < 3 {
		t.Errorf("want a Stop per completed turn, got %d", completed)
	}
	terminal, err := Start(sb.Repo, env, 40, 140, binary, "--conversation", conversation, "--model", model, "--dangerously-skip-permissions")
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
	invocations := len(sb.HookPayloads("antigravity-pre-invocation"))
	// agy moves a shell command to the background after five seconds and ends the turn,
	// so the cancel lands on the model's reply instead.
	if err := terminal.Type("Write a 1500-word essay on the history of lighthouses. Do not use any tools."); err != nil {
		t.Fatal(err)
	}
	var began time.Time
	cancelled, escaped := "", false
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline) && cancelled == ""; time.Sleep(250 * time.Millisecond) {
		if began.IsZero() && len(sb.HookPayloads("antigravity-pre-invocation")) > invocations {
			began = time.Now()
		}
		// While the model is answering.
		if !escaped && !began.IsZero() && time.Since(began) > 1500*time.Millisecond {
			escaped = terminal.Send("\x1b") == nil
		}
		if stops := sb.HookPayloads("antigravity-stop"); len(stops) > completed {
			cancelled, _ = stops[len(stops)-1]["terminationReason"].(string)
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
	// The model loads a skill by reading its SKILL.md: that read's tool call names the skill,
	// and what the skill says it is goes once.
	defs := sb.Delivered("terma.skill.definition", conversation, 10*time.Second)
	if len(defs) != 1 || defs[0].Attrs["terma.skill.name"] != "house-greeting" || defs[0].Attrs["terma.skill.origin"] != "repository" ||
		defs[0].Attrs["terma.skill.description"] != "Greets the person in the house style. Use when asked for a house greeting." {
		t.Errorf("want one definition of the house-greeting skill, got %+v", defs)
	}
	used := sb.Receiver.WaitLogs(time.Second, func(l LogRecord) bool {
		return l.EventName == "terma.tool.call" && l.Attrs["session.id"] == conversation && l.Attrs["terma.skill.name"] != ""
	})
	if len(used) == 0 || used[0].Attrs["terma.skill.name"] != "house-greeting" || used[0].Attrs["gen_ai.tool.name"] != "read_file" {
		t.Errorf("want the skill's read_file call to name it, got %+v", used)
	}
	touched := sb.Delivered("terma.files.touched", conversation, 10*time.Second)
	if len(touched) == 0 {
		t.Errorf("no terma.files.touched delivered for the written file")
	}
	if len(sb.Delivered("terma.commit.stamped", "", 10*time.Second)) == 0 {
		t.Errorf("no terma.commit.stamped delivered")
	}
}
