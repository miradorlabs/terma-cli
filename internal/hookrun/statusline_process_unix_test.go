//go:build unix

package hookrun

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStatusLineCancellationStopsRendererGroup(t *testing.T) {
	env, out, _ := statusEnv(t, quotaPayload)
	marker := filepath.Join(t.TempDir(), "child-survived")
	t.Setenv("TERMA_TEST_RENDER_MARKER", marker)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	code := StatusLine(ctx, env, StatusLineOptions{Renderer: `(sleep 1; printf leaked > "$TERMA_TEST_RENDER_MARKER") & wait`, Indicator: true})
	if code == 0 || time.Since(start) > 900*time.Millisecond || out.Len() != 0 {
		t.Fatalf("code=%d duration=%s output=%q", code, time.Since(start), out.String())
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("renderer grandchild survived cancellation")
	}
}

func TestStatusLineOwnTimeoutCapturesBeforeStoppingRenderer(t *testing.T) {
	t.Setenv("TERMA_STATUSLINE_TIMEOUT", "100ms")
	env, out, sp := statusEnv(t, quotaPayload)
	marker := filepath.Join(t.TempDir(), "child-survived-timeout")
	t.Setenv("TERMA_TEST_RENDER_MARKER", marker)
	captured := false
	started := time.Now()
	code := StatusLine(context.Background(), env, StatusLineOptions{
		Renderer:  `(sleep 1; printf leaked > "$TERMA_TEST_RENDER_MARKER") & wait`,
		Indicator: true,
		OnCapture: func() {
			if events := spooledQuota(t, sp); len(events) != 1 {
				t.Fatalf("capture must be deliverable while rendering: %+v", events)
			}
			captured = true
		},
	})
	if code != 124 || !captured || out.Len() != 0 || time.Since(started) > 900*time.Millisecond {
		t.Fatalf("code=%d captured=%v output=%q duration=%s", code, captured, out.String(), time.Since(started))
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("renderer child survived Terma's timeout")
	}
}

func TestStatusLineShellExitDoesNotLeaveBackgroundRenderer(t *testing.T) {
	env, _, _ := statusEnv(t, quotaPayload)
	marker := filepath.Join(t.TempDir(), "orphan-survived")
	t.Setenv("TERMA_TEST_RENDER_MARKER", marker)
	code := StatusLine(context.Background(), env, StatusLineOptions{
		Renderer:  `(sleep 1; printf leaked > "$TERMA_TEST_RENDER_MARKER") &`,
		Indicator: true,
	})
	if code == 0 {
		t.Fatal("unclosed renderer pipes must report failure")
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("background renderer survived shell exit and pipe-drain timeout")
	}
}
