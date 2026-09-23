package harness

import (
	"testing"
)

// OTEL_RESOURCE_ATTRIBUTES is the user's variable for describing their own resources.
// Terma never writes it for Claude Code: the key names the project, Claude Code stamps
// user.id and user.email itself, and its resource already carries
// service.name=claude-code. The exporter's attributes are for Codex and OpenCode.
func TestRenderNeverWritesResourceAttributes(t *testing.T) {
	for _, h := range []Harness{Claude{}, Claude{}.Local(t.TempDir())} {
		if got, ok := h.(Claude).render(fullExporter())[otelResourceAttributes]; ok {
			t.Fatalf("%s = %q rendered", otelResourceAttributes, got)
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
	if got := envOf(t, path)[otelResourceAttributes]; got != theirs {
		t.Fatalf("after connect %s = %q, want the user's value untouched", otelResourceAttributes, got)
	}
	st, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.ProjectID != "proj_123" {
		t.Fatalf("status project = %q, want the journal's proj_123 (not parsed from the user's attributes)", st.ProjectID)
	}
	if key, ok := c.CurrentCredential("https://otel.terma.ai", "proj_123"); !ok || key != "mir_srv_0123456789abcdef" {
		t.Fatalf("CurrentCredential = %q, %v; want the installed key reused for the journal's project", key, ok)
	}
	if _, ok := c.CurrentCredential("https://otel.terma.ai", "proj_other"); ok {
		t.Fatal("CurrentCredential reused the key for a different project")
	}

	if _, err := c.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if got := envOf(t, path)[otelResourceAttributes]; got != theirs {
		t.Fatalf("after disconnect %s = %q, want the user's value untouched", otelResourceAttributes, got)
	}
}

// A configuration written by an earlier Terma carries the project in
// OTEL_RESOURCE_ATTRIBUTES and a journal that owns that key. Status still reads the
// project from it, and the next connect removes it — it is Terma's leftover, not the
// user's — leaving the journal as the record.
func TestReconnectClearsResourceAttributesAnEarlierTermaWrote(t *testing.T) {
	c, path := claudeIn(t, `{}`)
	if err := c.Connect(fullExporter(), true); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Rewind to what an older Terma left behind: the variable in the file, owned by
	// the journal, and a journal that predates the project field.
	const legacy = "enduser.id=dev@example.com,mirador.project.id=proj_legacy,service.name=claude-code"
	s, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	s.env[otelResourceAttributes] = legacy
	if err := s.save(false); err != nil {
		t.Fatalf("save: %v", err)
	}
	j, err := loadJournal(c.Name(), path)
	if err != nil || j == nil {
		t.Fatalf("journal: %v, %v", j, err)
	}
	j.Installed[otelResourceAttributes] = legacy
	j.Previous[otelResourceAttributes] = nil
	j.ProjectID = ""
	if err := j.save(); err != nil {
		t.Fatalf("save journal: %v", err)
	}

	st, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.ProjectID != "proj_legacy" {
		t.Fatalf("legacy status project = %q, want proj_legacy read from the attributes", st.ProjectID)
	}

	if err := c.Connect(fullExporter(), true); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if got, ok := envOf(t, path)[otelResourceAttributes]; ok {
		t.Fatalf("%s = %q survived the reconnect; an earlier Terma's leftover must go", otelResourceAttributes, got)
	}
	st, err = c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.ProjectID != "proj_123" {
		t.Fatalf("status project = %q, want proj_123 from the journal", st.ProjectID)
	}
}
