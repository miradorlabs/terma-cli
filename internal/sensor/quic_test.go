package sensor

import (
	"encoding/hex"
	"testing"
)

// The Initial keys derive as RFC 9001's Appendix A.1 says they do.
func TestQUICInitialKeysMatchRFC9001(t *testing.T) {
	dcid, _ := hex.DecodeString("8394c8f03e515708")
	key, iv, hp := initialKeys(dcid)
	for _, c := range []struct{ name, got, want string }{
		{"key", hex.EncodeToString(key), "1f369613dd76d5467730efcbe3b1a22d"},
		{"iv", hex.EncodeToString(iv), "fa044b2f42a3fd3b46fb255c"},
		{"hp", hex.EncodeToString(hp), "9f50449e04a0e810283a1e9933adedd2"},
	} {
		if c.got != c.want {
			t.Errorf("client initial %s = %s, want %s", c.name, c.got, c.want)
		}
	}
}
