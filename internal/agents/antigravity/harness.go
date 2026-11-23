package antigravity

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sync"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

var agyVersionRE = regexp.MustCompile(`\d+\.\d+(\.\d+)?`)

// detections are the agy binaries already asked for their version, by path and file: doctor
// and setup ask once per agy, however many hooks they judge.
var detections sync.Map

func detect(ctx context.Context) harness.Detection {
	path, err := exec.LookPath("agy")
	if err != nil {
		return harness.Detection{}
	}
	info, err := os.Stat(path)
	if err != nil {
		return harness.DetectBinary(ctx, "agy", agyVersionRE)
	}
	key := fmt.Sprintf("%s|%d|%d", path, info.Size(), info.ModTime().UnixNano())
	if d, ok := detections.Load(key); ok {
		return d.(harness.Detection)
	}
	d := harness.DetectBinary(ctx, "agy", agyVersionRE)
	detections.Store(key, d)
	return d
}
