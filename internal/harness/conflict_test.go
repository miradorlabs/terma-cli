package harness

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const termaEndpoint = "https://otel.terma.ai"

// miradorExporter is the export a default connect installs: every signal, Terma's
// endpoint.
func termaExporter() Exporter {
	return Exporter{Endpoint: termaEndpoint, Signals: AllSignals}
}

// The finding this file exists for: Claude Code resolves each signal's exporter from
// the per-signal endpoint when one is set, and merges the *generic* headers into it. A
// connect that writes only the generic endpoint plus an Authorization header therefore
// hands Terma's server key to whoever owns that per-signal endpoint.
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

// The OTLP spec: a generic endpoint gets `v1/<signal>` appended, but a per-signal
// endpoint "MUST be used as-is without any modification". So only the suffixed URL is
// equivalent — and the bare base URL, which looks equivalent, posts to the wrong path.
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
		// It still goes to Terma's host, so it is a broken export rather than a
		// credential handed to a stranger.
		if conflicts[0].Credential {
			t.Error("Terma's own base URL is not a credential disclosure")
		}
	})
}

// A signal Terma is not exporting cannot be redirected away from Terma, so an
// override on it must not block the connect.
func TestConflictsIgnoreOverridesForDisabledSignals(t *testing.T) {
	c, _ := claudeIn(t, `{"env":{
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":"https://other-collector.example.com"
	}}`)

	conflicts, err := c.ConflictsWith(Exporter{
		Endpoint: termaEndpoint,
		Signals:  []Signal{SignalMetrics},
	})
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("got %+v, want none — traces are not being exported", conflicts)
	}
}

// Detailed beta tracing diverts logs and traces to its own endpoint instead of the
// exporters, so it defeats a connect without touching any OTEL_* variable. Terma
// writes user settings, which get none of the managed-settings protection that would
// otherwise strip it.
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

	conflicts, err := c.ConflictsWith(Exporter{
		Endpoint: termaEndpoint,
		Signals:  []Signal{SignalMetrics},
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
			// The switch alone does nothing, so leaving it is dead config.
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
		// A header bag may itself be a credential; the key is named, the value never is.
		if c.Key == "OTEL_EXPORTER_OTLP_LOGS_HEADERS" && c.Value != "" {
			t.Errorf("a header value was captured for display: %q", c.Value)
		}
	}
}

// A generic endpoint already exporting elsewhere is not a leak — it gets overwritten —
// but replacing someone's working collector must be a decision, not a side effect.
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
	// It is overwritten, so it is not a disclosure.
	if conflicts[0].Credential {
		t.Error("the generic endpoint is replaced by the connect; it is not a credential leak")
	}
}

// A destination configured while telemetry is switched off is still a destination
// someone chose, and connect overwrites it just the same. Ignoring it because nothing
// is currently exporting is how a disconnect-reconfigure-reconnect cycle silently
// destroys the replacement configuration.
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

// The finding this rule exists for: connect (backup A), disconnect, reconfigure to B,
// reconnect. A backup that is never overwritten still holds A, so the reconnect replaces
// B and the next disconnect deletes it — B unrecoverable. The backup must track the last
// non-Terma state, not the first one ever seen.
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

	// The user sets up a different collector while disconnected.
	s, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	s.env[otelEndpoint] = "https://collector-b.example.com"
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

// Connect must not silently delete the user's settings. Without clearConflicts the
// override survives, which is exactly why the command refuses to connect while it does.
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
	// Only the conflicts go. Everything else is still the user's.
	if env["EDITOR"] != "vim" {
		t.Error("--force removed an unrelated setting")
	}
	if env["OTEL_EXPORTER_OTLP_ENDPOINT"] != termaEndpoint {
		t.Errorf("generic endpoint = %q", env["OTEL_EXPORTER_OTLP_ENDPOINT"])
	}
}

// Status must not report a Terma endpoint while a per-signal override quietly sends
// that signal somewhere else.
func TestStatusReportsConflicts(t *testing.T) {
	c, _ := claudeIn(t, "")
	if err := c.Connect(fullExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Simulate an override added after the fact.
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

// Connect overwrites the telemetry variables and disconnect deletes them; neither
// remembers what was there. The backup is the only record of the user's original
// collector, so a re-connect must not replace it with a copy of Terma's own settings.
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

	// The second connect backs up a file that now contains Terma's settings.
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

// Anyone keeping ~/.claude/settings.json as a link into a dotfiles repo would find the
// link silently swapped for a regular file, detaching it from the repo.
func TestConnectWritesThroughASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}

	dir := t.TempDir()
	realDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())

	target := filepath.Join(realDir, "settings.json")
	if err := os.WriteFile(target, []byte(`{"model":"opus"}`), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	c := Claude{}
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
	// The content must have landed in the real file, not just anywhere.
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !strings.Contains(string(data), "CLAUDE_CODE_ENABLE_TELEMETRY") {
		t.Fatalf("the write did not reach the symlink target:\n%s", data)
	}

	// And the backup belongs next to the real file, not next to the link.
	if _, err := os.Stat(target + ".terma.bak"); err != nil {
		if _, err2 := c.Backup(termaEndpoint); err2 != nil {
			t.Fatalf("Backup: %v", err2)
		}
		if _, err := os.Stat(target + ".terma.bak"); err != nil {
			t.Errorf("backup was not written alongside the resolved file: %v", err)
		}
	}
}

// Telemetry switched off, or the endpoint deleted, leaves the server key on disk. That
// config is not "connected", and disconnect keying off Connected would walk away from it.
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

	// And disconnect must actually clear it.
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
	// The credential is the part that must not survive.
	if after.KeyPrefix != "" {
		t.Fatalf("the server key survived disconnect: %+v", after)
	}

	// The one key deliberately left behind is the switch the user themselves set to 0
	// after connecting. Its value is no longer what Terma installed, so it is their
	// edit — the same rule that stops disconnect deleting a collector someone
	// reconfigured. It is reported rather than silently kept.
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

// A key Terma installed and nobody touched is restored to whatever it held before —
// including "absent". A key edited since is left alone, which is what stops a disconnect
// deleting a collector somebody reconfigured after connecting.
func TestDisconnectRestoresPreviousValuesAndSkipsEdits(t *testing.T) {
	c, path := claudeIn(t, `{"env":{
		"OTEL_EXPORTER_OTLP_ENDPOINT":"https://their-collector.example.com",
		"OTEL_LOG_USER_PROMPTS":"1"
	}}`)

	if err := c.Connect(fullExporter(), true); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Somebody changes the export after connecting.
	s, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	s.env[otelProtocol] = "grpc"
	if err := s.save(false); err != nil {
		t.Fatalf("save: %v", err)
	}

	result, err := c.Disconnect()
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}

	env := envOf(t, path)
	// Present before Terma: restored to the original values, not deleted.
	if env[otelEndpoint] != "https://their-collector.example.com" {
		t.Errorf("%s = %q, want the pre-Terma collector restored", otelEndpoint, env[otelEndpoint])
	}
	if env[otelLogUserPrompts] != "1" {
		t.Errorf("%s = %q, want the pre-Terma value restored", otelLogUserPrompts, env[otelLogUserPrompts])
	}
	// Absent before Terma: removed.
	if _, ok := env[otelHeaders]; ok {
		t.Errorf("%s survived; it did not exist before the connect", otelHeaders)
	}
	// Edited after the connect: left alone and reported.
	if env[otelProtocol] != "grpc" {
		t.Errorf("%s = %q, want the later edit preserved", otelProtocol, env[otelProtocol])
	}
	if !slices.Contains(result.Skipped, otelProtocol) {
		t.Errorf("skipped = %v, want the edited key reported", result.Skipped)
	}
	if result.Restored == 0 {
		t.Error("nothing was reported as restored")
	}
}

// --force takes settings that were never Terma's. Disconnect gives them back.
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

// A reconnect updates what Terma installed, but it must not turn the preceding
// Terma installation into the value disconnect restores. The ownership chain starts
// before the first connect and survives until the final disconnect.
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
	if got := env[otelEndpoint]; got != "https://original.example.com" {
		t.Errorf("endpoint = %q, want the value from before the first connect", got)
	}
	if _, ok := env[otelHeaders]; ok {
		t.Error("a Terma credential survived the final disconnect")
	}
	if got := readJSON(t, path)[claudeOtelHeadersHelper]; got != "/usr/local/bin/headers.sh" {
		t.Errorf("otelHeadersHelper = %v, want the conflict cleared by the first connect restored", got)
	}
}

// A first disconnect deliberately leaves an edited value alone. Keeping a reduced
// journal makes that decision stable: status no longer calls the edit Terma-owned,
// and a repeated disconnect must keep leaving the edit alone.
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
	t.Setenv("TERMA_CONFIG_DIR", blocked)

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
	journalFile, err := journalPath(c.Name(), path)
	if err != nil {
		t.Fatalf("journalPath: %v", err)
	}
	if err := os.WriteFile(journalFile, []byte(`{"installed":`), 0o600); err != nil {
		t.Fatalf("corrupt journal: %v", err)
	}

	if _, err := c.Disconnect(); err == nil {
		t.Fatal("Disconnect treated a corrupt ownership journal as an absent journal")
	}
	if envOf(t, path)[otelHeaders] == "" {
		t.Error("Disconnect changed settings despite the corrupt ownership journal")
	}
}

func mustConfigPath(t *testing.T, c Claude) string {
	t.Helper()
	path, err := c.ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	return path
}

// Writing through a dangling link would replace the link with a regular file, silently
// detaching a dotfiles setup from its repo. There is no safe way to guess where the
// missing target belonged, so this is refused rather than papered over.
func TestConnectRefusesADanglingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())

	link := filepath.Join(dir, "settings.json")
	missing := filepath.Join(t.TempDir(), "not-checked-out", "settings.json")
	if err := os.Symlink(missing, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	c := Claude{}
	err := c.Connect(fullExporter(), false)
	if err == nil {
		t.Fatal("Connect followed a dangling symlink")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %q, want it to explain the broken link", err)
	}

	// The link itself must survive untouched.
	info, lerr := os.Lstat(link)
	if lerr != nil {
		t.Fatalf("lstat: %v", lerr)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the dangling link was replaced by a regular file")
	}
}

// Disconnecting a settings file that holds nothing but Terma's keys empties the
// document. Deleting the target of a link would leave the link dangling and break the
// next read, so a linked file is emptied to `{}` instead.
func TestDisconnectKeepsASymlinkTargetAlive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	dir := t.TempDir()
	realDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())

	target := filepath.Join(realDir, "settings.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	c := Claude{}
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
	// And the file must still be readable as settings.
	st, err := c.Status()
	if err != nil {
		t.Fatalf("Status after disconnect: %v", err)
	}
	if st.ManagedKeys != 0 {
		t.Errorf("managed keys survived: %d", st.ManagedKeys)
	}
}

// Claude Code resolves settings from several files, and the user file Terma writes is
// the lowest-precedence of them. A project file wins — so a conflict there decides where
// telemetry goes while the user file supplies Terma's Authorization header.
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
	if conflicts[0].Scope != ScopeProject {
		t.Errorf("scope = %q, want %q", conflicts[0].Scope, ScopeProject)
	}
	// Terma writes the user file; it has no business editing a project's settings, and
	// --force must not claim to have handled this.
	if conflicts[0].Clearable {
		t.Error("a project setting must not be reported as clearable")
	}
	if !conflicts[0].Credential {
		t.Error("a foreign per-signal endpoint is a credential disclosure wherever it is set")
	}
}

// A variable exported in the shell takes effect regardless of what is written to any
// settings file, and Terma cannot unset it for the user.
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
	if conflicts[0].Scope != ScopeEnvironment {
		t.Errorf("scope = %q, want %q", conflicts[0].Scope, ScopeEnvironment)
	}
	if conflicts[0].Clearable {
		t.Error("Terma cannot unset a shell export; it must not be reported as clearable")
	}
}

// An exported value that already agrees with what Terma would install is not a
// conflict — the check must not fire on its own configuration.
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

// otelHeadersHelper is a top-level setting, not an env entry, so a scan of the env block
// never sees it — while it decides what the export authenticates with.
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
	// It is in the user file, so --force can take it.
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

// A saved beta endpoint with the switch off does nothing, so blocking on it is a false
// positive that also talks --force into deleting two settings for no reason.
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

// Connecting a sandbox under CLAUDE_CONFIG_DIR must not disturb a different config's
// record. A single per-harness journal made these collide: the sandbox connect
// overwrote the record for the real ~/.claude, and every later read of the real config
// found a journal describing a file it was never asked about.
func TestJournalIsPerConfigNotPerHarness(t *testing.T) {
	termaHome := t.TempDir()
	t.Setenv("TERMA_CONFIG_DIR", termaHome)

	realDir := t.TempDir()
	sandboxDir := t.TempDir()
	c := Claude{}

	// Connect the "real" config.
	t.Setenv("CLAUDE_CONFIG_DIR", realDir)
	if err := c.Connect(fullExporter(), false); err != nil {
		t.Fatalf("connect real: %v", err)
	}

	// Connect a sandbox, as anyone testing this would.
	t.Setenv("CLAUDE_CONFIG_DIR", sandboxDir)
	sandbox := fullExporter()
	sandbox.Endpoint = "https://otel-dev.terma.ai"
	if err := c.Connect(sandbox, false); err != nil {
		t.Fatalf("connect sandbox: %v", err)
	}

	// The real config must still read cleanly, and still be connected.
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

	// And each disconnects independently, restoring its own file.
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

// A connect that switches the credential from inline to a headers helper — which is
// what happens when a machine connected by an earlier tool, or to another project, is
// pointed at a new one — must not leave the old inline header behind. The OTel SDK
// reads OTEL_EXPORTER_OTLP_HEADERS itself and it outranks the helper script, so a
// stale value keeps sending every export to the project the old key belonged to while
// `status` and `doctor` both report a healthy connection.
func TestConnectClearsInlineHeaderWhenSwitchingToHelper(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	// A developer's own CLAUDE_CONFIG_DIR would otherwise move the file this test reads.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, ".claude"))
	path := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	c := Claude{}
	old := Exporter{
		Endpoint:  "https://otel-dev.mirador.org",
		APIKey:    "ter_srv_oldprojectkey0001",
		ProjectID: "project-old",
		Signals:   AllSignals,
	}
	if err := c.Connect(old, true); err != nil {
		t.Fatalf("first connect: %v", err)
	}
	if got := envOf(t, path)[otelHeaders]; got == "" {
		t.Fatal("inline connect should have written the header")
	}

	helper := filepath.Join(dir, ".config", "terma", "helpers", "claude-otel-project-new")
	fresh := Exporter{
		Endpoint:   "https://otel-dev.mirador.org",
		APIKey:     "ter_srv_newprojectkey0002",
		ProjectID:  "project-new",
		Signals:    AllSignals,
		HelperPath: helper,
	}
	if err := c.Connect(fresh, true); err != nil {
		t.Fatalf("second connect: %v", err)
	}

	env := envOf(t, path)
	if got, ok := env[otelHeaders]; ok {
		t.Fatalf("%s survived the switch to helper mode with %q; exports would carry the old project's key", otelHeaders, got)
	}
	if got, ok := env[otelResourceAttributes]; ok {
		t.Fatalf("%s = %q written; the project travels in the journal, never in the user's resource attributes", otelResourceAttributes, got)
	}
	body, err := os.ReadFile(helper)
	if err != nil {
		t.Fatalf("helper: %v", err)
	}
	if !strings.Contains(string(body), "ter_srv_newprojectkey0002") {
		t.Fatal("helper does not carry the new key")
	}

	// The status the CLI reports must come from the helper, not a leftover.
	st, err := c.Status()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Connected || st.ProjectID != "project-new" {
		t.Fatalf("status = %+v, want connected to project-new", st)
	}
}

// Traces off must not leave the beta switch from an earlier traces-on connect behind:
// the same convergence rule, on a key whose staleness opts the user into a beta.
func TestConnectClearsBetaSwitchWhenTracesAreOff(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	// A developer's own CLAUDE_CONFIG_DIR would otherwise move the file this test reads.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, ".claude"))
	path := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	c := Claude{}
	if err := c.Connect(fullExporter(), true); err != nil {
		t.Fatalf("connect with traces: %v", err)
	}
	if envOf(t, path)[claudeEnhancedTelemetry] != "1" {
		t.Fatal("expected the beta switch after a traces connect")
	}

	noTraces := fullExporter()
	noTraces.Signals = []Signal{SignalLogs, SignalMetrics}
	if err := c.Connect(noTraces, true); err != nil {
		t.Fatalf("reconnect without traces: %v", err)
	}
	if got, ok := envOf(t, path)[claudeEnhancedTelemetry]; ok {
		t.Fatalf("%s survived a traces-off connect with %q", claudeEnhancedTelemetry, got)
	}
}
