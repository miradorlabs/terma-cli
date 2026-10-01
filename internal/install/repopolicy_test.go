package install

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents/builtin"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// policyFile is an exporter whose repository policy is a file under the repository.
type policyFile struct {
	path      string
	hasPolicy bool
	conflicts []harness.Conflict
}

func (policyFile) Name() string                             { return "fake" }
func (policyFile) DisplayName() string                      { return "Fake" }
func (policyFile) Detect(context.Context) harness.Detection { return harness.Detection{Found: true} }
func (p policyFile) ConfigPath() (string, error)            { return p.path, nil }
func (policyFile) SupportsHeadersHelper() bool              { return false }
func (p policyFile) Status() (harness.Status, error) {
	return harness.Status{HasPolicy: p.hasPolicy}, nil
}
func (p policyFile) ConflictsWith(harness.Exporter) ([]harness.Conflict, error) {
	return p.conflicts, nil
}
func (p policyFile) Connect(x harness.Exporter, _ bool) error {
	data, err := json.Marshal(x)
	if err != nil {
		return err
	}
	return os.WriteFile(p.path, data, 0o600)
}
func (policyFile) Disconnect() (harness.DisconnectResult, error) {
	return harness.DisconnectResult{}, nil
}
func (policyFile) Local(string) (harness.Harness, bool)            { return nil, false }
func (policyFile) CurrentCredential(string, string) (string, bool) { return "", false }
func (policyFile) Backup(string) (string, error)                   { return "", nil }
func (policyFile) ConnectNotes(harness.Exporter) []string          { return nil }

var _ harness.Harness = policyFile{}

// A repository policy is written once, kept on a re-install unless updated, and skipped
// with a next step when the repository already has settings of its own; an advisory
// conflict does not stop it.
func TestWriteRepoPolicy(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "policy.json")
	want := Plan{Prompts: true}.Exporter("http://ingest", []harness.Signal{harness.SignalLogs})
	advisory := []harness.Conflict{{Key: "OTEL_ADVISORY", Advisory: true}}

	var r report
	written, err := WriteRepoPolicy(&r, root, []harness.Harness{policyFile{path: path, conflicts: advisory}}, want, false)
	if err != nil || !slices.Equal(written, []string{"policy.json"}) || !slices.Equal(r.ok, []string{"Repo policy"}) {
		t.Fatalf("first write = %v, %v, report %+v", written, err, r)
	}
	if written, err := WriteRepoPolicy(&r, root, []harness.Harness{policyFile{path: path}}, want, true); err != nil || written != nil {
		t.Fatalf("an unchanged update = %v, %v", written, err)
	}
	if written, err := WriteRepoPolicy(&r, root, []harness.Harness{policyFile{path: path, hasPolicy: true}}, want, false); err != nil || written != nil {
		t.Fatalf("an existing policy was rewritten: %v, %v", written, err)
	}

	r = report{}
	blocked := policyFile{path: filepath.Join(root, "other.json"), conflicts: []harness.Conflict{{Key: "OTEL_EXPORTER_OTLP_ENDPOINT"}}}
	if written, err := WriteRepoPolicy(&r, root, []harness.Harness{blocked}, want, false); err != nil || written != nil || len(r.warn) != 1 || len(r.then) != 1 {
		t.Fatalf("a conflict = %v, %v, report %+v", written, err, r)
	}
	if _, err := os.Stat(blocked.path); !os.IsNotExist(err) {
		t.Fatalf("wrote over a conflict: %v", err)
	}
}

// The routing record carries the plan's choices for the agents that route through the
// relay, and there is none when no selected agent does.
func TestRouteRecord(t *testing.T) {
	reg := builtin.Agents()
	want := Plan{Prompts: true}.Exporter("http://ingest", []harness.Signal{harness.SignalLogs})
	rec, ok := RouteRecord(reg, "proj_1", []string{"claude"}, want)
	if !ok || rec.ProjectID != "proj_1" || rec.Endpoint != "http://ingest" || !slices.Equal(rec.Signals, []string{"logs"}) ||
		!rec.IncludePrompts || rec.IncludeToolContent || !slices.Equal(rec.Harnesses, []string{"claude"}) {
		t.Fatalf("RouteRecord = %+v, %v", rec, ok)
	}
	if _, ok := RouteRecord(reg, "proj_1", nil, want); ok {
		t.Fatal("a record with no agent to route")
	}
}
