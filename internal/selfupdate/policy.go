package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// Policy is what the latest release says about every installation: the oldest version
// still supported. It is release/policy.json in the repository, published with each
// release and covered by the release's signed checksums.txt, so raising the minimum is a
// commit and a release — no backend involved.
type Policy struct {
	MinVersion string `json:"min_version"`
}

// PolicyFile names the policy asset of a release.
const PolicyFile = "policy.json"

// PolicyInterval is how often a running relay reads the latest policy. The files are
// fetched from the release download host, not the rate-limited API.
const PolicyInterval = time.Hour

// downloadBase is where the latest release's files are downloaded from by name.
func (c *Client) downloadBase() string {
	if c.DownloadURL != "" {
		return strings.TrimRight(c.DownloadURL, "/")
	}
	return "https://github.com/" + Repo + "/releases/latest/download"
}

// FetchPolicy reads the latest release's policy.json, trusting it only when its digest
// is in a checksums.txt whose signature verifies.
func (c *Client) FetchPolicy(ctx context.Context) (Policy, error) {
	base := c.downloadBase()
	sums, err := c.get(ctx, base+"/checksums.txt")
	if err != nil {
		return Policy{}, err
	}
	sig, err := c.get(ctx, base+"/checksums.txt"+SignatureSuffix)
	if err != nil {
		return Policy{}, fmt.Errorf("%w: %w", ErrUnsigned, err)
	}
	if err := Verify(sums, sig); err != nil {
		return Policy{}, err
	}
	want := ParseChecksums(bytes.NewReader(sums))[PolicyFile]
	if want == "" {
		return Policy{}, errors.New("the latest release has no policy")
	}
	body, err := c.get(ctx, base+"/"+PolicyFile)
	if err != nil {
		return Policy{}, err
	}
	got := sha256.Sum256(body)
	if hex.EncodeToString(got[:]) != want {
		return Policy{}, errors.New("policy.json does not match the signed checksums")
	}
	var p Policy
	if err := json.Unmarshal(body, &p); err != nil {
		return Policy{}, fmt.Errorf("read policy.json: %w", err)
	}
	if p.MinVersion != "" && !IsRelease(p.MinVersion) {
		return Policy{}, fmt.Errorf("policy.json names %q, which is not a version", p.MinVersion)
	}
	return p, nil
}

// BelowMinimum reports whether current is a release older than the policy's minimum.
// A source build is never below it: it is never replaced either.
func BelowMinimum(current string, p Policy) bool {
	return IsRelease(current) && IsRelease(p.MinVersion) && Newer(current, p.MinVersion)
}

const policyFile = "update-policy.json"

// policyCache is the last policy a relay read, for commands that must not reach the
// network to warn about it.
type policyCache struct {
	CheckedAt time.Time `json:"checked_at"`
	Policy
}

// SavePolicy records the latest policy for MinimumWarning to read.
func SavePolicy(dir string, p Policy) {
	_ = config.WriteJSON(filepath.Join(dir, policyFile), policyCache{CheckedAt: time.Now(), Policy: p}, 0o600)
}

// MinimumWarning is the warning for an installation below the recorded minimum, "" when
// it is not. It reads only the local record, so any command can afford it.
func MinimumWarning(dir, current string) string {
	var c policyCache
	data, err := os.ReadFile(filepath.Join(dir, policyFile))
	if err != nil || json.Unmarshal(data, &c) != nil || !BelowMinimum(current, c.Policy) {
		return ""
	}
	return fmt.Sprintf("terma %s is older than the oldest supported version (%s). Run `terma update`.", current, c.MinVersion)
}
