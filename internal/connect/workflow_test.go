package connect

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// exporter is a made-up agent's exporter that records what a connect asked of it.
type exporter struct {
	log        *[]string
	conflicts  []harness.Conflict
	connectErr error
	wrote      *harness.Exporter
}

func (exporter) Name() string                             { return "fake" }
func (exporter) DisplayName() string                      { return "Fake" }
func (exporter) Detect(context.Context) harness.Detection { return harness.Detection{Found: true} }
func (exporter) ConfigPath() (string, error)              { return "/home/dev/.fake/settings.json", nil }
func (exporter) SupportsHeadersHelper() bool              { return false }
func (exporter) Status() (harness.Status, error)          { return harness.Status{}, nil }
func (e exporter) ConflictsWith(harness.Exporter) ([]harness.Conflict, error) {
	return e.conflicts, nil
}
func (e exporter) Connect(x harness.Exporter, _ bool) error {
	*e.log = append(*e.log, "connect")
	*e.wrote = x
	return e.connectErr
}
func (exporter) Disconnect() (harness.DisconnectResult, error) {
	return harness.DisconnectResult{}, nil
}
func (exporter) Local(string) (harness.Harness, bool)            { return nil, false }
func (exporter) CurrentCredential(string, string) (string, bool) { return "", false }
func (e exporter) Backup(string) (string, error) {
	*e.log = append(*e.log, "backup")
	return "", nil
}
func (exporter) ConnectNotes(harness.Exporter) []string { return nil }

var _ harness.Harness = exporter{}

func steps(log *[]string, confirm bool) Steps {
	return Steps{
		Confirm: func(string) (bool, error) { *log = append(*log, "confirm"); return confirm, nil },
		Key: func(context.Context) (Key, error) {
			*log = append(*log, "mint")
			return Key{Value: "ter_srv_minted", Prefix: "ter_srv_m…", Minted: true}, nil
		},
		Store: func(string) error { *log = append(*log, "store"); return nil },
	}
}

func fake(log *[]string) (exporter, *harness.Exporter) {
	wrote := &harness.Exporter{}
	return exporter{log: log, wrote: wrote}, wrote
}

var cfg = &config.Config{OTLPURL: "https://otlp.example", ProjectID: "p1"}

// A connect asks before it mints, mints before it writes, and stores the key it wrote.
func TestAGlobalConnectAsksThenMintsThenWrites(t *testing.T) {
	var log []string
	h, wrote := fake(&log)
	var out bytes.Buffer
	if err := Global(t.Context(), agents.New(), h, cfg, Options{Identity: "none"}, steps(&log, true), IO{&out, &out}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"confirm", "mint", "backup", "connect", "store"}; !slices.Equal(log, want) {
		t.Fatalf("steps = %v, want %v", log, want)
	}
	if wrote.APIKey != "ter_srv_minted" || wrote.Endpoint != cfg.OTLPURL {
		t.Fatalf("wrote %+v", wrote)
	}
}

// A declined question, or a conflict terma may not clear, mints no key and writes nothing.
func TestAGlobalConnectThatStopsMintsNothing(t *testing.T) {
	var log []string
	h, _ := fake(&log)
	if err := Global(t.Context(), agents.New(), h, cfg, Options{Identity: "none"}, steps(&log, false), IO{&bytes.Buffer{}, &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(log, []string{"confirm"}) {
		t.Fatalf("a declined connect went on: %v", log)
	}
	log = nil
	h.conflicts = []harness.Conflict{{Key: "OTEL_EXPORTER_OTLP_ENDPOINT", Scope: "shell"}}
	if err := Global(t.Context(), agents.New(), h, cfg, Options{Identity: "none", Force: true}, steps(&log, true), IO{&bytes.Buffer{}, &bytes.Buffer{}}); err == nil {
		t.Fatal("a shell override was connected over")
	}
	if len(log) != 0 {
		t.Fatalf("a refused connect went on: %v", log)
	}
}

// A write that fails after a key was minted says to revoke it.
func TestAFailedWriteNamesTheMintedKey(t *testing.T) {
	var log []string
	h, _ := fake(&log)
	h.connectErr = errors.New("read-only file system")
	var errOut bytes.Buffer
	if err := Global(t.Context(), agents.New(), h, cfg, Options{Identity: "none", AssumeYes: true}, steps(&log, true), IO{&bytes.Buffer{}, &errOut}); err == nil {
		t.Fatal("a failed write succeeded")
	}
	if !strings.Contains(errOut.String(), "ter_srv_m…") || slices.Contains(log, "store") {
		t.Fatalf("stderr %q, steps %v", errOut.String(), log)
	}
}

// A repository's policy carries what it ships and never a key.
func TestALocalConnectWritesNoKey(t *testing.T) {
	var log []string
	global, _ := fake(&log)
	local, wrote := fake(&log)
	if err := Local(t.Context(), global, local, cfg, t.TempDir(), Options{Signals: []harness.Signal{harness.SignalTraces}, AssumeYes: true}, Steps{}, IO{&bytes.Buffer{}, &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	if wrote.APIKey != "" || len(wrote.Signals) != 1 || !slices.Equal(log, []string{"connect"}) {
		t.Fatalf("wrote %+v, steps %v", wrote, log)
	}
}
