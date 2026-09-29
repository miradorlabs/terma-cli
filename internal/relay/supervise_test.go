package relay

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestSupervisedChild is the process Supervise starts in these tests: it exits at once.
func TestSupervisedChild(t *testing.T) {
	if os.Getenv("TERMA_RELAY_SUPERVISED_CHILD") != "1" {
		t.Skip("run only as a supervised child")
	}
	os.Exit(3)
}

func TestSuperviseStartsTheRelayAgainAfterItExits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var starts atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		Supervise(ctx, func(ctx context.Context) *exec.Cmd {
			if starts.Add(1) == 2 {
				cancel() // the second start is the proof; stop there
			}
			c := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSupervisedChild$")
			c.Env = append(os.Environ(), "TERMA_RELAY_SUPERVISED_CHILD=1")
			return c
		}, t.Logf)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Supervise did not return when its context ended")
	}
	if n := starts.Load(); n < 2 {
		t.Fatalf("started %d times; a relay that exits must be started again", n)
	}
}

func TestWindowsLauncherQuotesAndRoundTrips(t *testing.T) {
	binary := `C:\Users\A B\AppData\Local\terma & co\terma.exe`
	vbs := string(windowsLauncher(binary, [][2]string{{"TERMA_ENV", `d"ev`}}))
	for _, want := range []string{
		`Set sh = CreateObject("WScript.Shell")`,
		`env("TERMA_ENV") = "d""ev"`,
		`, 0, False`,
	} {
		if !strings.Contains(vbs, want) {
			t.Errorf("launcher lacks %q:\n%s", want, vbs)
		}
	}
	if got := launcherBinary([]byte(vbs)); got != binary {
		t.Fatalf("binary read back as %q from\n%s", got, vbs)
	}
}
