package install

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// policyFile is an exporter whose repository policy is a file under the repository.
type policyFile struct {
	path      string
	hasPolicy bool
	// stale is a content switch an earlier terma wrote beside signals.
	stale     bool
	signals   []harness.Signal
	conflicts []harness.Conflict
}

func (policyFile) Name() string                             { return "fake" }
func (policyFile) DisplayName() string                      { return "Fake" }
func (policyFile) Detect(context.Context) harness.Detection { return harness.Detection{Found: true} }
func (p policyFile) ConfigPath() (string, error)            { return p.path, nil }

func (p policyFile) Status() (harness.Status, error) {
	return harness.Status{HasPolicy: p.hasPolicy, StaleContent: p.stale, Signals: p.signals}, nil
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

var _ harness.Harness = policyFile{}

// A repository policy is written once, kept on a re-install unless updated, and skipped
// with a next step when the repository already has settings of its own; an advisory
// conflict does not stop it.
func TestWriteRepoPolicy(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "policy.json")
	want := Plan{}.Exporter("http://ingest", []harness.Signal{harness.SignalLogs})
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

// A repository policy holding an earlier terma's content switch is rewritten even unasked,
// with its own signals: content is the team policy's alone.
func TestWriteRepoPolicyRemovesAStaleContentSwitch(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "policy.json")
	if err := os.WriteFile(path, []byte(`{"stale":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := policyFile{path: path, hasPolicy: true, stale: true, signals: []harness.Signal{harness.SignalTraces}}
	var r report
	written, err := WriteRepoPolicy(&r, root, []harness.Harness{stale}, Plan{}.Exporter("http://ingest", harness.AllSignals), false)
	if err != nil || !slices.Equal(written, []string{"policy.json"}) {
		t.Fatalf("a stale content switch was kept: %v, %v", written, err)
	}
	var got harness.Exporter
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &got); err != nil || !slices.Equal(got.Signals, []harness.Signal{harness.SignalTraces}) {
		t.Fatalf("rewrote %s, want the repository's own signals kept", data)
	}
}
