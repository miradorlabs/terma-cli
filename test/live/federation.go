package live

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Each Claude process needs a fresh assertion: GitHub JWTs are single-use at
// Anthropic's exchange endpoint. Claude performs the exchange itself, keeping
// federation on the Console billing route rather than subscription OAuth.
func githubClaudeFederation(dir string) ([]string, error) {
	var env []string
	for _, key := range []string{"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_ORGANIZATION_ID", "ANTHROPIC_SERVICE_ACCOUNT_ID", "ANTHROPIC_WORKSPACE_ID"} {
		value := os.Getenv(key)
		if value == "" {
			return nil, fmt.Errorf("missing %s", key)
		}
		env = append(env, key+"="+value)
	}
	token, err := githubIdentityToken(&http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL"), os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN"))
	if err != nil {
		return nil, err
	}
	// Mask before Claude can emit diagnostics. Never retain assertions in reports.
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		fmt.Printf("::add-mask::%s\n", token)
	}
	f, err := os.CreateTemp(dir, "anthropic-identity-*.jwt")
	if err != nil {
		return nil, fmt.Errorf("create identity file: %w", err)
	}
	_, writeErr := f.WriteString(token)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		return nil, fmt.Errorf("write identity file failed")
	}
	path, err := filepath.Abs(f.Name())
	if err != nil {
		return nil, err
	}
	return append(env, "ANTHROPIC_IDENTITY_TOKEN_FILE="+path), nil
}

func githubIdentityToken(client *http.Client, endpoint, bearer string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || bearer == "" {
		return "", fmt.Errorf("GitHub OIDC unavailable; requires id-token: write")
	}
	q := u.Query()
	q.Set("audience", "https://api.anthropic.com")
	u.RawQuery = q.Encode()
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("invalid GitHub OIDC request")
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := client.Do(req)
	if err != nil {
		// HTTP errors can include the request URL and its sensitive query.
		return "", fmt.Errorf("GitHub OIDC request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub OIDC returned HTTP %d", resp.StatusCode)
	}
	var result struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil || result.Value == "" || strings.ContainsAny(result.Value, "\r\n%") {
		return "", fmt.Errorf("GitHub OIDC returned an invalid token response")
	}
	return result.Value, nil
}
