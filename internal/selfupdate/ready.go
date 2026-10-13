package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"time"
)

// checksumsName is the release asset naming every archive's digest (ParseChecksums).
const checksumsName = "checksums.txt"

// ErrNotReady means a release is published but not whole: its archive for this platform,
// its checksums or their signature is not on it. GitHub names a release the latest from the
// moment it is published, while its files come from a build that starts only once a reviewer
// approves it and then takes minutes; the signature is made nowhere else, as the signing
// key is that build's alone. So a release without them has not been through approval yet:
// it is asked about again at the next pass, rather than tried and then held back as an
// install that failed (AttemptInterval).
var ErrNotReady = errors.New("the release is still being built and signed; try again in a few minutes")

// head asks github.com for the page at path without following where it redirects, which is
// the answer. The pages are not held to the API's 60 requests an hour per address, so what
// is asked at every pass is asked of them (latestTag, built).
func (c *Client) head(ctx context.Context, path string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.web()+"/"+Repo+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgentPrefix+c.Version)
	client := c.httpClient()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	return resp, nil
}

// built reports whether the release of version carries every file an install of it needs
// (PickAsset), asking for each as a download rather than the API for the release: a release
// may wait a long while for its approval, and every machine looking it up at each pass
// meanwhile would spend the lookups its address is given. The signature is asked for first:
// a build makes it last, so one request answers for a release still waiting.
func (c *Client) built(ctx context.Context, version string) (bool, error) {
	for _, name := range []string{SignatureName, checksumsName, AssetName(runtime.GOOS, runtime.GOARCH)} {
		resp, err := c.head(ctx, "/releases/download/v"+version+"/"+name)
		switch {
		case err != nil:
			return false, err
		case resp.StatusCode == http.StatusNotFound:
			return false, nil
		case resp.StatusCode >= http.StatusBadRequest:
			return false, fmt.Errorf("github release download: HTTP %d", resp.StatusCode)
		}
	}
	return true, nil
}

// current is the release installed where this process started: its own, or the one it has
// installed in its place since. A process that keeps running after it installed a release,
// as the relay does until its agents pause, compares the next release to that one.
func (c *Client) current() string {
	if c.installed != "" {
		return c.installed
	}
	return c.Version
}

// adopt has this process go on as version, which it just installed at exe: what is
// installed is no longer what it started from but is its own doing, so the next release is
// installed over it rather than refused as another install's (Replaced).
func (c *Client) adopt(version, exe string) {
	if info, err := os.Stat(exe); err == nil {
		c.installed, c.Binary = version, info
	}
}
