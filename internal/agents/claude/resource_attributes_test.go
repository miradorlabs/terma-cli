package claude

import (
	"os"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// OTEL_RESOURCE_ATTRIBUTES is the user's variable for describing their own resources.
// Terma never writes it for Claude Code: the key names the project, Claude Code stamps
// user.id and user.email itself, and its resource already carries
// service.name=claude-code. The exporter's attributes are for Codex and OpenCode.
func TestRenderNeverWritesResourceAttributes(t *testing.T) {
	for _, h := range []harness.Harness{exporter{}, exporter{}.Local(t.TempDir())} {
		if got, ok := h.(exporter).render(fullExporter())[harness.EnvResourceAttributes]; ok {
			t.Fatalf("%s = %q rendered", harness.EnvResourceAttributes, got)
		}
	}
}

// A user's own resource attributes are neither overwritten by a connect nor removed by
// the disconnect that follows: they were never Terma's.
func TestConnectLeavesUsersResourceAttributesAlone(t *testing.T) {
	const theirs = "deployment.environment=staging,team=payments"
	c, path := claudeIn(t, `{"env":{"OTEL_RESOURCE_ATTRIBUTES":"`+theirs+`"}}`)

	if err := c.Connect(fullExporter(), true); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got := envOf(t, path)[harness.EnvResourceAttributes]; got != theirs {
		t.Fatalf("after connect %s = %q, want the user's value untouched", harness.EnvResourceAttributes, got)
	}
	st, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.ProjectID != "proj_123" {
		t.Fatalf("status project = %q, want the journal's proj_123 (not parsed from the user's attributes)", st.ProjectID)
	}
	if key, ok := c.CurrentCredential("https://otel.terma.ai", "proj_123"); !ok || key != "ter_srv_0123456789abcdef" {
		t.Fatalf("CurrentCredential = %q, %v; want the installed key reused for the journal's project", key, ok)
	}
	if _, ok := c.CurrentCredential("https://otel.terma.ai", "proj_other"); ok {
		t.Fatal("CurrentCredential reused the key for a different project")
	}

	if _, err := c.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if got := envOf(t, path)[harness.EnvResourceAttributes]; got != theirs {
		t.Fatalf("after disconnect %s = %q, want the user's value untouched", harness.EnvResourceAttributes, got)
	}
}

func TestUnjournaledUserSettingsAreNotOwned(t *testing.T) {
	const original = `{"env":{"CLAUDE_CODE_ENABLE_TELEMETRY":"1","OTEL_EXPORTER_OTLP_ENDPOINT":"https://collector.example","OTEL_RESOURCE_ATTRIBUTES":"mirador.project.id=unselected"}}`
	c, path := claudeIn(t, original)
	st, err := c.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.ManagedKeys != 0 || st.ProjectID != "" {
		t.Fatalf("unowned configuration claimed: %+v", st)
	}
	result, err := c.Disconnect()
	if err != nil || result.Removed != 0 || result.Restored != 0 {
		t.Fatalf("unowned configuration changed: %+v, %v", result, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != original {
		t.Fatalf("user settings changed: %q, %v", data, err)
	}
}
