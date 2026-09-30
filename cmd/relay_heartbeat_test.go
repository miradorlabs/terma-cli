package cmd

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// The heartbeat's facts name this terma's build and setup, never whose machine it is:
// no hostname, no home directory, no account. The machine id is random and stays put.
func TestHeartbeatFactsNameNoOne(t *testing.T) {
	dir := relaySandbox(t)
	facts := heartbeatFacts(dir)
	for _, k := range []string{"terma.version", "terma.os", "terma.arch", "terma.machine_id", "terma.install", "terma.mode", "terma.relay.service"} {
		if _, ok := facts[k]; !ok {
			t.Errorf("no %s in %v", k, facts)
		}
	}
	host, _ := os.Hostname()
	home, _ := os.UserHomeDir()
	for k, v := range facts {
		s := fmt.Sprint(v)
		if host != "" && strings.Contains(s, host) || home != "" && strings.Contains(s, home) || strings.Contains(s, "@") {
			t.Errorf("%s = %q names the machine or its owner", k, s)
		}
	}
	id := facts["terma.machine_id"]
	if id == "" || heartbeatFacts(dir)["terma.machine_id"] != id {
		t.Fatalf("machine id %q is not stable", id)
	}
}
