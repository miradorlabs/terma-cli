package antigravity

import (
	"context"
	"regexp"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

var agyVersionRE = regexp.MustCompile(`\d+\.\d+(\.\d+)?`)

func detect(ctx context.Context) harness.Detection {
	return harness.DetectBinary(ctx, "agy", agyVersionRE)
}
