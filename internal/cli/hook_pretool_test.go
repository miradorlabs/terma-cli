package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// On a machine not yet migrated nothing is classified, so Claude Code's PreToolUse hook
// for a read or a shell command returns before locating any repository or handing on the
// records a sandboxed git hook parked there, and dispatch claims nothing from a payload it
// never read. An edit's PostToolUse in the same checkout still does both.
func TestAReadHookOnAMachineNotMigratedDoesNothing(t *testing.T) {
	if _, ok := testApp.agents.Event("pre-tool-use"); !ok {
		t.Fatal("no agent handles pre-tool-use")
	}
	root := collectedRepo(t)
	store := filepath.Join(root, ".git", termaproject.GitStoreDir)
	// What a git hook parks when the state directory refuses it (hookrun's parkedEvents).
	if err := session.Open(store).Park("events", []byte(`{"name":"terma.commit","time":"`+time.Now().UTC().Format(time.RFC3339)+`"}`+"\n")); err != nil {
		t.Fatal(err)
	}
	parked := func() bool {
		_, err := os.Stat(filepath.Join(store, "events.jsonl"))
		return err == nil
	}
	const sid = "sess-pre-tool-use"
	for _, tool := range []string{`"Read","tool_input":{"file_path":"` + hookruntest.InJSON(filepath.Join(root, "README.md")) + `"}`,
		`"Bash","tool_input":{"command":"cat README.md"}`} {
		runHook(t, "pre-tool-use", `{"session_id":"`+sid+`","cwd":"`+hookruntest.InJSON(root)+`","hook_event_name":"PreToolUse","tool_name":`+tool+`}`)
	}
	if c, ok := claim.Read(testApp.stateDir, sid, time.Now()); ok {
		t.Errorf("the PreToolUse hook claimed its session: %+v", c)
	}
	if !parked() {
		t.Fatal("the PreToolUse hook handed on the parked records")
	}

	runHook(t, "post-tool-use", `{"session_id":"`+sid+`","cwd":"`+hookruntest.InJSON(root)+`","hook_event_name":"PostToolUse","tool_name":"Write","tool_input":{"file_path":"`+
		hookruntest.InJSON(filepath.Join(root, "a.txt"))+`"}}`)
	if _, ok := claim.Read(testApp.stateDir, sid, time.Now()); !ok || parked() {
		t.Fatalf("an edit's hook: claimed %v, records still parked %v", ok, parked())
	}
}
