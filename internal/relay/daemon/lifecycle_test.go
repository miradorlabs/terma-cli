package daemon

import "testing"

// A Go test binary is no relay, on Windows too, where it ends .test.exe: started as one, it
// would run its package's tests detached and hold the relay's log open.
func TestATestBinaryIsNoRelay(t *testing.T) {
	for exe, want := range map[string]bool{
		"/tmp/go-build1/b001/daemon.test":        true,
		`C:\Temp\go-build1\b001\daemon.test.exe`: true,
		"/usr/local/bin/terma":                   false,
		`C:\Program Files\terma\terma.exe`:       false,
		"/opt/latest.tester":                     false,
	} {
		if got := TestBinary(exe); got != want {
			t.Errorf("TestBinary(%q) = %v, want %v", exe, got, want)
		}
	}
}
