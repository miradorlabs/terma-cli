package daemon

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A relay logs its start and its exit with the counters, and its exit counters no longer
// replace the previous relay's: they move to stats.prev.json.
func TestARelayLogsItsRunAndKeepsThePreviousCounters(t *testing.T) {
	dir, _ := setUpRelay(t)
	for range 2 {
		c := runConfig(dir, time.Millisecond, nil)
		c.Log = NewLog(dir)
		run(t, c)
		c.Log.Close()
	}
	data, err := os.ReadFile(filepath.Join(dir, LogFile))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if strings.Count(log, "listening on") != 2 || strings.Count(log, "stopped (stopped or idle)") != 2 {
		t.Fatalf("relay log:\n%s", log)
	}
	for _, name := range []string{StatsFile, PrevStatsFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A relay that cannot listen says so in its log, not only in last-error.
func TestARelayLogsWhyItCouldNotListen(t *testing.T) {
	dir, _ := setUpRelay(t)
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	c := runConfig(dir, time.Hour, nil)
	c.Addr = taken.Addr().String()
	c.Log = NewLog(dir)
	if _, err := Run(t.Context(), c); err == nil {
		t.Fatal("Run on a taken port succeeded")
	}
	c.Log.Close()
	if data, _ := os.ReadFile(filepath.Join(dir, LogFile)); !strings.Contains(string(data), "listen on "+c.Addr) {
		t.Fatalf("relay log = %q", data)
	}
}

// The log stays bounded: past maxLog it moves to relay.log.1, replacing the older one.
func TestTheRelayLogIsBounded(t *testing.T) {
	dir := t.TempDir()
	l := NewLog(dir)
	defer l.Close()
	line := strings.Repeat("x", 1000)
	for range 3 * maxLog / 1000 {
		l.Printf("%s", line)
	}
	for _, name := range []string{LogFile, LogFile + ".1"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > maxLog+2000 {
			t.Errorf("%s is %d bytes, over the %d bound", name, info.Size(), maxLog)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, LogFile+"*")); len(matches) != 2 {
		t.Fatalf("log files = %v, want the log and one older generation", matches)
	}
}
