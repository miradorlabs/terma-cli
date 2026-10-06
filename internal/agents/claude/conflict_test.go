package claude

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

const termaEndpoint = "https://otel.terma.ai"

func termaExporter() harness.Exporter {
	return harness.Exporter{Endpoint: termaEndpoint, Signals: harness.AllSignals}
}

// A per-signal endpoint receives the merged generic headers, so it would get terma's key.
func TestConflictsDetectPerSignalEndpointLeak(t *testing.T) {
	c, _ := claudeIn(t, `{"env":{
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":"https://other-collector.example.com"
	}}`)

	conflicts, err := c.ConflictsWith(termaExporter())
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("got %d conflicts, want 1: %+v", len(conflicts), conflicts)
	}
	if conflicts[0].Key != "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT" {
		t.Errorf("conflict key = %q", conflicts[0].Key)
	}
	if !conflicts[0].Credential {
		t.Error("a foreign per-signal endpoint must be flagged as a credential disclosure")
	}
}

// A per-signal endpoint is used as-is (OTLP spec), so only the suffixed URL is equivalent.
func TestConflictsComparePerSignalEndpointAgainstTheSignalPath(t *testing.T) {
	t.Run("suffixed url is equivalent", func(t *testing.T) {
		c, _ := claudeIn(t, `{"env":{
			"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":"`+termaEndpoint+`/v1/traces"
		}}`)

		conflicts, err := c.ConflictsWith(termaExporter())
		if err != nil {
			t.Fatalf("ConflictsWith: %v", err)
		}
		if len(conflicts) != 0 {
			t.Fatalf("got %+v, want none — this is exactly where the generic endpoint sends traces", conflicts)
		}
	})

	t.Run("bare base url posts to the wrong path", func(t *testing.T) {
		c, _ := claudeIn(t, `{"env":{
			"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":"`+termaEndpoint+`"
		}}`)

		conflicts, err := c.ConflictsWith(termaExporter())
		if err != nil {
			t.Fatalf("ConflictsWith: %v", err)
		}
		if len(conflicts) != 1 {
			t.Fatalf("got %+v, want the bare base URL reported — no /v1/traces is appended to a per-signal endpoint", conflicts)
		}
		// Terma's own host: a broken export, not a credential handed to a stranger.
		if conflicts[0].Credential {
			t.Error("Terma's own base URL is not a credential disclosure")
		}
	})
}

// An override on a signal terma is not exporting must not block the connect.
func TestConflictsIgnoreOverridesForDisabledSignals(t *testing.T) {
	c, _ := claudeIn(t, `{"env":{
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":"https://other-collector.example.com"
	}}`)

	conflicts, err := c.ConflictsWith(harness.Exporter{
		Endpoint: termaEndpoint,
		Signals:  []harness.Signal{harness.SignalMetrics},
	})
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("got %+v, want none — traces are not being exported", conflicts)
	}
}

// Detailed beta tracing diverts logs and traces without touching any OTEL_* variable.
func TestConflictsDetectBetaTracingRedirect(t *testing.T) {
	c, _ := claudeIn(t, `{"env":{
		"ENABLE_BETA_TRACING_DETAILED":"1",
		"BETA_TRACING_ENDPOINT":"https://beta-collector.example.com"
	}}`)

	conflicts, err := c.ConflictsWith(termaExporter())
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if len(conflicts) != 1 || conflicts[0].Key != "BETA_TRACING_ENDPOINT" {
		t.Fatalf("got %+v, want the beta tracing redirect reported", conflicts)
	}
	if !conflicts[0].Credential {
		t.Error("a redirect of logs and traces must be treated as a credential disclosure")
	}
}

// Beta tracing moves only logs and traces; a metrics-only connect is unaffected.
func TestConflictsIgnoreBetaTracingForMetricsOnly(t *testing.T) {
	c, _ := claudeIn(t, `{"env":{
		"BETA_TRACING_ENDPOINT":"https://beta-collector.example.com"
	}}`)

	conflicts, err := c.ConflictsWith(harness.Exporter{
		Endpoint: termaEndpoint,
		Signals:  []harness.Signal{harness.SignalMetrics},
	})
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("got %+v, want none — beta tracing does not move metrics", conflicts)
	}
}

func TestConnectClearsBetaTracingPairWhenAsked(t *testing.T) {
	c, path := claudeIn(t, `{"env":{
		"ENABLE_BETA_TRACING_DETAILED":"1",
		"BETA_TRACING_ENDPOINT":"https://beta-collector.example.com"
	}}`)

	if err := c.Connect(fullExporter(), true); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	env := envOf(t, path)
	for _, key := range []string{"BETA_TRACING_ENDPOINT", "ENABLE_BETA_TRACING_DETAILED"} {
		if _, ok := env[key]; ok {
			t.Errorf("%s survived --force", key)
		}
	}
}

func TestConflictsDetectPerSignalHeadersAndProtocol(t *testing.T) {
	c, _ := claudeIn(t, `{"env":{
		"OTEL_EXPORTER_OTLP_LOGS_HEADERS":"Authorization=Bearer someone-elses-key",
		"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL":"grpc"
	}}`)

	conflicts, err := c.ConflictsWith(termaExporter())
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if len(conflicts) != 2 {
		t.Fatalf("got %d conflicts, want 2: %+v", len(conflicts), conflicts)
	}

	for _, c := range conflicts {
		if c.Key == "OTEL_EXPORTER_OTLP_LOGS_HEADERS" && c.Value != "" {
			t.Errorf("a header value was captured for display: %q", c.Value)
		}
	}
}

// Replacing someone's working collector must be a decision, not a side effect.
func TestConflictsReportExistingGenericEndpoint(t *testing.T) {
	c, _ := claudeIn(t, `{"env":{
		"CLAUDE_CODE_ENABLE_TELEMETRY":"1",
		"OTEL_EXPORTER_OTLP_ENDPOINT":"https://their-collector.example.com"
	}}`)

	conflicts, err := c.ConflictsWith(termaExporter())
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if len(conflicts) != 1 || conflicts[0].Key != "OTEL_EXPORTER_OTLP_ENDPOINT" {
		t.Fatalf("got %+v, want the existing generic endpoint reported", conflicts)
	}
	if conflicts[0].Credential {
		t.Error("the generic endpoint is replaced by the connect; it is not a credential leak")
	}
}

// A destination saved while telemetry is off is still someone's choice, and connect overwrites it.
func TestConflictsReportGenericEndpointEvenWhenTelemetryIsOff(t *testing.T) {
	c, _ := claudeIn(t, `{"env":{
		"CLAUDE_CODE_ENABLE_TELEMETRY":"0",
		"OTEL_EXPORTER_OTLP_ENDPOINT":"https://their-collector.example.com"
	}}`)

	conflicts, err := c.ConflictsWith(termaExporter())
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if len(conflicts) != 1 || conflicts[0].Key != "OTEL_EXPORTER_OTLP_ENDPOINT" {
		t.Fatalf("got %+v, want the dormant destination reported", conflicts)
	}
	if !strings.Contains(conflicts[0].Reason, "previously configured") {
		t.Errorf("reason = %q, want it to say the export is not currently live", conflicts[0].Reason)
	}
}

// After connect, disconnect, reconfigure and reconnect, the backup holds the latest non-terma
// state, or the next disconnect deletes the reconfiguration.
func TestBackupTracksTheLatestNonMiradorConfiguration(t *testing.T) {
	c, path := claudeIn(t, `{"env":{"OTEL_EXPORTER_OTLP_ENDPOINT":"https://collector-a.example.com"}}`)

	if _, err := c.Backup(termaEndpoint); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if err := c.Connect(fullExporter(), true); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := c.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}

	s, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	s.env[harness.EnvOTLPEndpoint] = "https://collector-b.example.com"
	if err := s.save(false); err != nil {
		t.Fatalf("save: %v", err)
	}

	backup, err := c.Backup(termaEndpoint)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	data, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if !strings.Contains(string(data), "collector-b") {
		t.Fatalf("backup holds a stale configuration; B is about to be overwritten and lost:\n%s", data)
	}
}

// Without clearConflicts the override survives, which is why the command refuses to connect.
func TestConnectLeavesConflictsAloneByDefault(t *testing.T) {
	c, path := claudeIn(t, `{"env":{
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":"https://other-collector.example.com"
	}}`)

	if err := c.Connect(fullExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got := envOf(t, path)["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"]; got != "https://other-collector.example.com" {
		t.Fatalf("the override was removed without being asked: %q", got)
	}
}

func TestConnectClearsConflictsWhenAsked(t *testing.T) {
	c, path := claudeIn(t, `{"env":{
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":"https://other-collector.example.com",
		"OTEL_EXPORTER_OTLP_LOGS_HEADERS":"Authorization=Bearer someone-elses-key",
		"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL":"grpc",
		"EDITOR":"vim"
	}}`)

	if err := c.Connect(fullExporter(), true); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	env := envOf(t, path)
	for _, key := range []string{
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_LOGS_HEADERS",
		"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL",
	} {
		if _, ok := env[key]; ok {
			t.Errorf("%s survived --force", key)
		}
	}
	if env["EDITOR"] != "vim" {
		t.Error("--force removed an unrelated setting")
	}
	if env["OTEL_EXPORTER_OTLP_ENDPOINT"] != termaEndpoint {
		t.Errorf("generic endpoint = %q", env["OTEL_EXPORTER_OTLP_ENDPOINT"])
	}
}

// Status reports a per-signal override that sends a signal away from terma.
func TestStatusReportsConflicts(t *testing.T) {
	c, _ := claudeIn(t, "")
	if err := c.Connect(fullExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	s, err := loadSettings(mustConfigPath(t, c))
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	s.env["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] = "https://other-collector.example.com"
	if err := s.save(false); err != nil {
		t.Fatalf("save: %v", err)
	}

	st, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(st.Conflicts) != 1 {
		t.Fatalf("status reported %d conflicts, want 1: %+v", len(st.Conflicts), st.Conflicts)
	}
	if !st.Conflicts[0].Credential {
		t.Error("status did not flag the override as a credential disclosure")
	}
}

// The backup is the only record of the user's original collector; a reconnect must not replace it.
func TestBackupIsNotOverwrittenByAReconnect(t *testing.T) {
	const original = `{"env":{"OTEL_EXPORTER_OTLP_ENDPOINT":"https://their-collector.example.com"}}`
	c, _ := claudeIn(t, original)

	first, err := c.Backup(termaEndpoint)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if err := c.Connect(fullExporter(), true); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	second, err := c.Backup(termaEndpoint)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if second != first {
		t.Fatalf("a second backup path was created (%q vs %q)", second, first)
	}

	data, err := os.ReadFile(first)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if !strings.Contains(string(data), "their-collector.example.com") {
		t.Fatalf("the backup no longer holds the original configuration:\n%s", data)
	}
	if strings.Contains(string(data), "ter_srv_") {
		t.Fatal("the backup was replaced with a copy of Terma's own settings")
	}
}

// A dotfiles link must stay a link.
func TestConnectWritesThroughASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}

	dir := t.TempDir()
	realDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	configDir := t.TempDir()

	target := filepath.Join(realDir, "settings.json")
	if err := os.WriteFile(target, []byte(`{"model":"opus"}`), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	c := exporter{dir: configDir}
	if err := c.Connect(fullExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a regular file; a dotfiles link would be broken")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !strings.Contains(string(data), "CLAUDE_CODE_ENABLE_TELEMETRY") {
		t.Fatalf("the write did not reach the symlink target:\n%s", data)
	}

	// The backup belongs next to the real file, not the link.
	if _, err := os.Stat(target + ".terma.bak"); err != nil {
		if _, err2 := c.Backup(termaEndpoint); err2 != nil {
			t.Fatalf("Backup: %v", err2)
		}
		if _, err := os.Stat(target + ".terma.bak"); err != nil {
			t.Errorf("backup was not written alongside the resolved file: %v", err)
		}
	}
}

// A config with telemetry off or no endpoint still holds the key, and disconnect must clear it.
func TestStatusCountsManagedKeysWhenNotConnected(t *testing.T) {
	c, _ := claudeIn(t, "")
	if err := c.Connect(fullExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	s, err := loadSettings(mustConfigPath(t, c))
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	s.env[claudeEnableTelemetry] = "0"
	if err := s.save(false); err != nil {
		t.Fatalf("save: %v", err)
	}

	st, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Connected {
		t.Fatal("telemetry is off; this must not report as connected")
	}
	if st.ManagedKeys == 0 {
		t.Fatal("no managed keys counted, so disconnect would leave the server key on disk")
	}
	if st.KeyPrefix == "" {
		t.Error("the key is still configured and should still be reported")
	}

	result, err := c.Disconnect()
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if result.Removed+result.Restored == 0 {
		t.Fatal("disconnect changed nothing in a config that still held the key")
	}

	after, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if after.KeyPrefix != "" {
		t.Fatalf("the server key survived disconnect: %+v", after)
	}

	// The switch the user set to 0 after connecting is their edit: kept and reported.
	if !slices.Contains(result.Skipped, claudeEnableTelemetry) {
		t.Errorf("skipped = %v, want the user-edited switch reported", result.Skipped)
	}
	env := envOf(t, mustConfigPath(t, c))
	if env[claudeEnableTelemetry] != "0" {
		t.Errorf("%s = %q, want the user's own value preserved", claudeEnableTelemetry, env[claudeEnableTelemetry])
	}
	for _, key := range claudeManagedKeys {
		if key == claudeEnableTelemetry {
			continue
		}
		if _, ok := env[key]; ok {
			t.Errorf("%s survived disconnect", key)
		}
	}
}

// An untouched key is restored to its prior value, absent included; an edited one is left alone.
func TestDisconnectRestoresPreviousValuesAndSkipsEdits(t *testing.T) {
	c, path := claudeIn(t, `{"env":{
		"OTEL_EXPORTER_OTLP_ENDPOINT":"https://their-collector.example.com",
		"OTEL_LOG_USER_PROMPTS":"1"
	}}`)

	if err := c.Connect(fullExporter(), true); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	s, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	s.env[harness.EnvOTLPProtocol] = "grpc"
	if err := s.save(false); err != nil {
		t.Fatalf("save: %v", err)
	}

	result, err := c.Disconnect()
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}

	env := envOf(t, path)
	if env[harness.EnvOTLPEndpoint] != "https://their-collector.example.com" {
		t.Errorf("%s = %q, want the pre-Terma collector restored", harness.EnvOTLPEndpoint, env[harness.EnvOTLPEndpoint])
	}
	if env[otelLogUserPrompts] != "1" {
		t.Errorf("%s = %q, want the pre-Terma value restored", otelLogUserPrompts, env[otelLogUserPrompts])
	}
	if _, ok := env[harness.EnvOTLPHeaders]; ok {
		t.Errorf("%s survived; it did not exist before the connect", harness.EnvOTLPHeaders)
	}
	if env[harness.EnvOTLPProtocol] != "grpc" {
		t.Errorf("%s = %q, want the later edit preserved", harness.EnvOTLPProtocol, env[harness.EnvOTLPProtocol])
	}
	if !slices.Contains(result.Skipped, harness.EnvOTLPProtocol) {
		t.Errorf("skipped = %v, want the edited key reported", result.Skipped)
	}
	if result.Restored == 0 {
		t.Error("nothing was reported as restored")
	}
}

// --force takes settings that were never terma's; disconnect gives them back.
func TestDisconnectRestoresClearedConflicts(t *testing.T) {
	c, path := claudeIn(t, `{
		"otelHeadersHelper": "/usr/local/bin/headers.sh",
		"env":{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":"https://other-collector.example.com"}
	}`)

	if err := c.Connect(fullExporter(), true); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, ok := envOf(t, path)["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"]; ok {
		t.Fatal("--force did not clear the override")
	}

	if _, err := c.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}

	if got := envOf(t, path)["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"]; got != "https://other-collector.example.com" {
		t.Errorf("cleared override = %q, want it restored", got)
	}
	if got := readJSON(t, path)["otelHeadersHelper"]; got != "/usr/local/bin/headers.sh" {
		t.Errorf("otelHeadersHelper = %v, want it restored", got)
	}
	if _, ok := envOf(t, path)[claudeOtelHeadersHelper]; ok {
		t.Error("otelHeadersHelper was also restored inside env")
	}
}

// A reconnect keeps the ownership chain that started before the first connect.
func TestReconnectPreservesOriginalJournal(t *testing.T) {
	c, path := claudeIn(t, `{
		"otelHeadersHelper":"/usr/local/bin/headers.sh",
		"env":{"OTEL_EXPORTER_OTLP_ENDPOINT":"https://original.example.com"}
	}`)

	first := fullExporter()
	first.APIKey = "ter_srv_first_credential"
	if err := c.Connect(first, true); err != nil {
		t.Fatalf("first Connect: %v", err)
	}

	second := fullExporter()
	second.APIKey = "ter_srv_second_credential"
	if err := c.Connect(second, true); err != nil {
		t.Fatalf("second Connect: %v", err)
	}

	if _, err := c.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	env := envOf(t, path)
	if got := env[harness.EnvOTLPEndpoint]; got != "https://original.example.com" {
		t.Errorf("endpoint = %q, want the value from before the first connect", got)
	}
	if _, ok := env[harness.EnvOTLPHeaders]; ok {
		t.Error("a Terma credential survived the final disconnect")
	}
	if got := readJSON(t, path)[claudeOtelHeadersHelper]; got != "/usr/local/bin/headers.sh" {
		t.Errorf("otelHeadersHelper = %v, want the conflict cleared by the first connect restored", got)
	}
}

// A reduced journal keeps an edited value alone across repeated disconnects.
func TestRepeatedDisconnectKeepsEditedKeys(t *testing.T) {
	c, path := claudeIn(t, `{}`)
	if err := c.Connect(fullExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	s, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	s.env[claudeEnableTelemetry] = "0"
	if err := s.save(false); err != nil {
		t.Fatalf("save edit: %v", err)
	}

	first, err := c.Disconnect()
	if err != nil {
		t.Fatalf("first Disconnect: %v", err)
	}
	if !slices.Contains(first.Skipped, claudeEnableTelemetry) {
		t.Fatalf("first skipped = %v, want the edited switch", first.Skipped)
	}
	status, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.ManagedKeys != 0 {
		t.Fatalf("ManagedKeys = %d, want the edited value excluded from Terma ownership", status.ManagedKeys)
	}

	second, err := c.Disconnect()
	if err != nil {
		t.Fatalf("second Disconnect: %v", err)
	}
	if second.Removed != 0 || second.Restored != 0 {
		t.Fatalf("second Disconnect changed the edit: %+v", second)
	}
	if got := envOf(t, path)[claudeEnableTelemetry]; got != "0" {
		t.Errorf("edited switch = %q after second disconnect, want 0", got)
	}
}

func TestConnectDoesNotWriteSettingsWhenJournalCannotBeSaved(t *testing.T) {
	const original = `{"model":"opus"}`
	c, path := claudeIn(t, original)

	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("blocked"), 0o600); err != nil {
		t.Fatalf("seed blocked journal parent: %v", err)
	}
	c.dir = blocked

	if err := c.Connect(fullExporter(), false); err == nil {
		t.Fatal("Connect succeeded without being able to save its ownership journal")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if string(data) != original {
		t.Fatalf("settings changed before the journal was durable:\n%s", data)
	}
}

func TestDisconnectRefusesCorruptJournal(t *testing.T) {
	c, path := claudeIn(t, `{}`)
	if err := c.Connect(fullExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	journalFile := harness.JournalPath(c.dir, c.Name(), path)
	if err := os.WriteFile(journalFile, []byte(`{"installed":`), 0o600); err != nil {
		t.Fatalf("corrupt journal: %v", err)
	}

	if _, err := c.Disconnect(); err == nil {
		t.Fatal("Disconnect treated a corrupt ownership journal as an absent journal")
	}
	if envOf(t, path)[harness.EnvOTLPHeaders] == "" {
		t.Error("Disconnect changed settings despite the corrupt ownership journal")
	}
}

func mustConfigPath(t *testing.T, c exporter) string {
	t.Helper()
	path, err := c.ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	return path
}

// Writing through a dangling link would replace it with a regular file, so it is refused.
func TestConnectRefusesADanglingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	configDir := t.TempDir()

	link := filepath.Join(dir, "settings.json")
	missing := filepath.Join(t.TempDir(), "not-checked-out", "settings.json")
	if err := os.Symlink(missing, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	c := exporter{dir: configDir}
	err := c.Connect(fullExporter(), false)
	if err == nil {
		t.Fatal("Connect followed a dangling symlink")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %q, want it to explain the broken link", err)
	}

	info, lerr := os.Lstat(link)
	if lerr != nil {
		t.Fatalf("lstat: %v", lerr)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the dangling link was replaced by a regular file")
	}
}

// A linked file emptied by disconnect becomes `{}`, so the link does not dangle.
func TestDisconnectKeepsASymlinkTargetAlive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	dir := t.TempDir()
	realDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	configDir := t.TempDir()

	target := filepath.Join(realDir, "settings.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	c := exporter{dir: configDir}
	if err := c.Connect(fullExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := c.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}

	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the symlink target was deleted, leaving the link dangling: %v", err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced by a regular file")
	}
	st, err := c.Status()
	if err != nil {
		t.Fatalf("Status after disconnect: %v", err)
	}
	if st.ManagedKeys != 0 {
		t.Errorf("managed keys survived: %d", st.ManagedKeys)
	}
}

// A project file outranks the user file, so its redirect is a conflict.
func TestConflictsDetectProjectSettings(t *testing.T) {
	c, _ := claudeIn(t, "")

	// A repository root, so the upward walk stops here.
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".claude", "settings.json"),
		[]byte(`{"env":{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":"https://project-collector.example.com"}}`), 0o644); err != nil {
		t.Fatalf("seed project settings: %v", err)
	}
	t.Chdir(repo)

	conflicts, err := c.ConflictsWith(termaExporter())
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("got %+v, want the project override reported", conflicts)
	}
	if conflicts[0].Scope != harness.ScopeProject {
		t.Errorf("scope = %q, want %q", conflicts[0].Scope, harness.ScopeProject)
	}
	// A project's settings are not terma's to edit, and --force must not claim to handle them.
	if conflicts[0].Clearable {
		t.Error("a project setting must not be reported as clearable")
	}
	if !conflicts[0].Credential {
		t.Error("a foreign per-signal endpoint is a credential disclosure wherever it is set")
	}
}

// A shell export wins over any settings file, and terma cannot unset it.
func TestConflictsDetectShellEnvironment(t *testing.T) {
	c, _ := claudeIn(t, "")
	t.Chdir(t.TempDir())
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "https://shell-collector.example.com")

	conflicts, err := c.ConflictsWith(termaExporter())
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("got %+v, want the exported variable reported", conflicts)
	}
	if conflicts[0].Scope != harness.ScopeEnvironment {
		t.Errorf("scope = %q, want %q", conflicts[0].Scope, harness.ScopeEnvironment)
	}
	if conflicts[0].Clearable {
		t.Error("Terma cannot unset a shell export; it must not be reported as clearable")
	}
}

// An export that agrees with what terma would install is not a conflict.
func TestConflictsIgnoreMatchingShellEnvironment(t *testing.T) {
	c, _ := claudeIn(t, "")
	t.Chdir(t.TempDir())
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", termaEndpoint+"/v1/traces")

	conflicts, err := c.ConflictsWith(termaExporter())
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("got %+v, want none", conflicts)
	}
}

// otelHeadersHelper is a top-level setting that decides what the export authenticates with.
func TestConflictsDetectOtelHeadersHelper(t *testing.T) {
	c, _ := claudeIn(t, `{"otelHeadersHelper":"/usr/local/bin/headers.sh"}`)
	t.Chdir(t.TempDir())

	conflicts, err := c.ConflictsWith(termaExporter())
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if len(conflicts) != 1 || conflicts[0].Key != "otelHeadersHelper" {
		t.Fatalf("got %+v, want the headers helper reported", conflicts)
	}
	if !conflicts[0].Credential {
		t.Error("a helper that supplies the Authorization header is a credential conflict")
	}
	if !conflicts[0].Clearable {
		t.Error("a helper in the user's own settings should be clearable")
	}
}

func TestConnectClearsOtelHeadersHelperWhenAsked(t *testing.T) {
	c, path := claudeIn(t, `{"model":"opus","otelHeadersHelper":"/usr/local/bin/headers.sh"}`)
	t.Chdir(t.TempDir())

	if err := c.Connect(fullExporter(), true); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	doc := readJSON(t, path)
	if _, ok := doc["otelHeadersHelper"]; ok {
		t.Error("otelHeadersHelper survived --force and would still supply the headers")
	}
	if doc["model"] != "opus" {
		t.Error("--force removed an unrelated top-level setting")
	}
}

// A saved beta endpoint with the switch off does nothing, so it must not block.
func TestConflictsIgnoreDormantBetaTracing(t *testing.T) {
	c, _ := claudeIn(t, `{"env":{
		"BETA_TRACING_ENDPOINT":"https://beta-collector.example.com"
	}}`)
	t.Chdir(t.TempDir())

	conflicts, err := c.ConflictsWith(termaExporter())
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("got %+v, want none — ENABLE_BETA_TRACING_DETAILED is not set", conflicts)
	}
}

// A sandbox under CLAUDE_CONFIG_DIR keeps its own journal, apart from the real config's.
func TestJournalIsPerConfigNotPerHarness(t *testing.T) {
	termaHome := t.TempDir()

	realDir := t.TempDir()
	sandboxDir := t.TempDir()
	c := exporter{dir: termaHome}

	t.Setenv("CLAUDE_CONFIG_DIR", realDir)
	if err := c.Connect(fullExporter(), false); err != nil {
		t.Fatalf("connect real: %v", err)
	}

	t.Setenv("CLAUDE_CONFIG_DIR", sandboxDir)
	sandbox := fullExporter()
	sandbox.Endpoint = "https://otel-dev.terma.ai"
	if err := c.Connect(sandbox, false); err != nil {
		t.Fatalf("connect sandbox: %v", err)
	}

	t.Setenv("CLAUDE_CONFIG_DIR", realDir)
	st, err := c.Status()
	if err != nil {
		t.Fatalf("Status on the real config after a sandbox connect: %v", err)
	}
	if !st.Connected {
		t.Fatal("the real config stopped reporting as connected")
	}
	if st.Endpoint != termaEndpoint {
		t.Errorf("real endpoint = %q, want it untouched by the sandbox connect", st.Endpoint)
	}

	realResult, err := c.Disconnect()
	if err != nil {
		t.Fatalf("disconnect real: %v", err)
	}
	if realResult.Unjournaled {
		t.Error("the real config lost its ownership record to the sandbox connect")
	}

	t.Setenv("CLAUDE_CONFIG_DIR", sandboxDir)
	sandboxStatus, err := c.Status()
	if err != nil {
		t.Fatalf("Status on the sandbox after the real disconnect: %v", err)
	}
	if !sandboxStatus.Connected {
		t.Fatal("disconnecting the real config also disconnected the sandbox")
	}
	sandboxResult, err := c.Disconnect()
	if err != nil {
		t.Fatalf("disconnect sandbox: %v", err)
	}
	if sandboxResult.Unjournaled {
		t.Error("the sandbox lost its own ownership record")
	}
}

// Switching to a headers helper clears the old inline header, which outranks the helper and would
// keep exporting to the old project.
func TestConnectClearsInlineHeaderWhenSwitchingToHelper(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	// A developer's own CLAUDE_CONFIG_DIR would move the file this test reads.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, ".claude"))
	path := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	c := exporter{dir: t.TempDir()}
	old := harness.Exporter{
		Endpoint:  "https://otel-dev.terma.ai",
		APIKey:    "ter_srv_oldprojectkey0001",
		ProjectID: "project-old",
		Signals:   harness.AllSignals,
	}
	if err := c.Connect(old, true); err != nil {
		t.Fatalf("first connect: %v", err)
	}
	if got := envOf(t, path)[harness.EnvOTLPHeaders]; got == "" {
		t.Fatal("inline connect should have written the header")
	}

	helper := filepath.Join(dir, ".config", "terma", "helpers", "claude-otel-project-new")
	fresh := harness.Exporter{
		Endpoint:   "https://otel-dev.terma.ai",
		APIKey:     "ter_srv_newprojectkey0002",
		ProjectID:  "project-new",
		Signals:    harness.AllSignals,
		HelperPath: helper,
	}
	if err := c.Connect(fresh, true); err != nil {
		t.Fatalf("second connect: %v", err)
	}

	env := envOf(t, path)
	if got, ok := env[harness.EnvOTLPHeaders]; ok {
		t.Fatalf("%s survived the switch to helper mode with %q; exports would carry the old project's key", harness.EnvOTLPHeaders, got)
	}
	if got, ok := env[harness.EnvResourceAttributes]; ok {
		t.Fatalf("%s = %q written; the project travels in the journal, never in the user's resource attributes", harness.EnvResourceAttributes, got)
	}
	body, err := os.ReadFile(helper)
	if err != nil {
		t.Fatalf("helper: %v", err)
	}
	if !strings.Contains(string(body), "ter_srv_newprojectkey0002") {
		t.Fatal("helper does not carry the new key")
	}

	st, err := c.Status()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Connected || st.ProjectID != "project-new" {
		t.Fatalf("status = %+v, want connected to project-new", st)
	}
}

// Traces off clears a beta switch an earlier traces-on connect left.
func TestConnectClearsBetaSwitchWhenTracesAreOff(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	// A developer's own CLAUDE_CONFIG_DIR would move the file this test reads.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, ".claude"))
	path := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	c := exporter{dir: t.TempDir()}
	if err := c.Connect(fullExporter(), true); err != nil {
		t.Fatalf("connect with traces: %v", err)
	}
	if envOf(t, path)[claudeEnhancedTelemetry] != "1" {
		t.Fatal("expected the beta switch after a traces connect")
	}

	noTraces := fullExporter()
	noTraces.Signals = []harness.Signal{harness.SignalLogs, harness.SignalMetrics}
	if err := c.Connect(noTraces, true); err != nil {
		t.Fatalf("reconnect without traces: %v", err)
	}
	if got, ok := envOf(t, path)[claudeEnhancedTelemetry]; ok {
		t.Fatalf("%s survived a traces-off connect with %q", claudeEnhancedTelemetry, got)
	}
}
