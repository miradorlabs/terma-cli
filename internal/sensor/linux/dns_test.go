//go:build linux

package linux

import "testing"

// DNSHash hashes the wire form, lowercase, from the last byte back, as dns_ingress does.
func TestDNSHashMatchesTheKernelsSuffixHash(t *testing.T) {
	// The kernel hashes a question's suffix "\x05sslip\x02io" byte by byte, backwards.
	wire := []byte("\x05sslip\x02io")
	var want uint64
	for i := len(wire) - 1; i >= 0; i-- {
		want = want*1099511628211 + uint64(wire[i])
	}
	if got := DNSHash("SSLIP.io."); got != want {
		t.Errorf("DNSHash = %x, want %x", got, want)
	}
}
