package cursorbilling

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const DefaultBaseURL = "https://api.cursor.com"

type Config struct {
	APIKey     string `json:"-"`
	Scope      Scope
	HTTPClient *http.Client
	// BaseURL is an operator setting for trusted gateways/tests, never tenant input.
	BaseURL          string
	PageSize         int
	MaxPages         int
	MaxRecords       int
	MaxResponseBytes int64
}
type Client struct {
	key, base                      string
	scope                          Scope
	http                           *http.Client
	pageSize, maxPages, maxRecords int
	maxBytes                       int64
}

func New(cfg Config) (*Client, error) {
	if err := cfg.Scope.validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.APIKey) == "" || strings.ContainsAny(cfg.APIKey, ":\r\n") {
		return nil, fmt.Errorf("valid API key required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("invalid base URL")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return nil, fmt.Errorf("HTTPS required except loopback tests")
	}
	if cfg.PageSize == 0 {
		cfg.PageSize = 1000
	}
	if cfg.MaxPages == 0 {
		cfg.MaxPages = 1000
	}
	if cfg.MaxRecords == 0 {
		cfg.MaxRecords = 100000
	}
	if cfg.MaxResponseBytes == 0 {
		cfg.MaxResponseBytes = 8 << 20
	}
	if cfg.PageSize < 1 || cfg.PageSize > 1000 || cfg.MaxPages < 1 || cfg.MaxPages > 10000 || cfg.MaxRecords < 1 || cfg.MaxRecords > 1000000 || cfg.MaxResponseBytes < 1 || cfg.MaxResponseBytes > 64<<20 {
		return nil, fmt.Errorf("invalid import bounds")
	}
	hc := http.Client{Timeout: 30 * time.Second}
	if cfg.HTTPClient != nil {
		hc = *cfg.HTTPClient
		if hc.Timeout == 0 {
			hc.Timeout = 30 * time.Second
		}
	}
	// Even same-host redirects may point to a write endpoint. No redirects follow.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{key: cfg.APIKey, base: strings.TrimRight(cfg.BaseURL, "/"), scope: cfg.Scope, http: &hc, pageSize: cfg.PageSize, maxPages: cfg.MaxPages, maxRecords: cfg.MaxRecords, maxBytes: cfg.MaxResponseBytes}, nil
}

// HTTPError excludes response bodies and credentials. Let the platform reschedule
// 429/5xx using RetryAfter; this library never retries or advances a checkpoint.
type HTTPError struct {
	Endpoint   string
	Status     int
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("Cursor %s returned HTTP %d", e.Endpoint, e.Status)
}
func (e *HTTPError) Retryable() bool { return e.Status == 429 || e.Status >= 500 }

func (c *Client) request(ctx context.Context, path string, body any) ([]byte, error) {
	method := http.MethodGet
	var b []byte
	if body != nil {
		method = http.MethodPost
		var err error
		b, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("invalid request")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("invalid request")
	}
	req.SetBasicAuth(c.key, "")
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("Cursor transport failed for %s", path)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		retry := time.Duration(0)
		raw := resp.Header.Get("Retry-After")
		if n, err := strconv.ParseInt(raw, 10, 32); err == nil && n > 0 {
			retry = time.Duration(n) * time.Second
		} else if at, err := http.ParseTime(raw); err == nil {
			retry = time.Until(at)
			if retry < 0 {
				retry = 0
			}
		}
		return nil, &HTTPError{Endpoint: path, Status: resp.StatusCode, RetryAfter: retry}
	}
	b, err = io.ReadAll(io.LimitReader(resp.Body, c.maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("Cursor response read failed for %s", path)
	}
	if int64(len(b)) > c.maxBytes {
		return nil, fmt.Errorf("Cursor response exceeds size bound for %s", path)
	}
	if !json.Valid(b) {
		return nil, fmt.Errorf("Cursor returned invalid JSON for %s", path)
	}
	return b, nil
}

// decode never incorporates provider values into errors, even for schema failures.
func decode(raw []byte, dst any) error {
	if err := json.Unmarshal(raw, dst); err != nil {
		return errors.New("Cursor response schema invalid")
	}
	return nil
}
func digest(raw []byte) string { return fmt.Sprintf("%x", sha256.Sum256(raw)) }
func canonical(raw []byte) ([]byte, error) {
	var v any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		return nil, fmt.Errorf("invalid JSON record")
	}
	return json.Marshal(v)
}

func (c *Client) Members(ctx context.Context) (MembersSnapshot, error) {
	out := MembersSnapshot{Scope: c.scope, Members: []Member{}}
	raw, err := c.request(ctx, "/teams/members", nil)
	if err != nil {
		return MembersSnapshot{}, err
	}
	var data struct {
		Rows *[]json.RawMessage `json:"teamMembers"`
	}
	if err = decode(raw, &data); err != nil || data.Rows == nil {
		return MembersSnapshot{}, fmt.Errorf("Cursor members response lacks valid teamMembers")
	}
	if len(*data.Rows) > c.maxRecords {
		return MembersSnapshot{}, fmt.Errorf("Cursor members exceeds record bound")
	}
	seen := map[string]bool{}
	for _, r := range *data.Rows {
		var m Member
		if err := decode(r, &m); err != nil {
			return MembersSnapshot{}, err
		}
		if m.ID == "" || seen[m.ID] {
			return MembersSnapshot{}, fmt.Errorf("Cursor members identity missing or repeated")
		}
		seen[m.ID] = true
		m.Raw = append(json.RawMessage(nil), r...)
		out.Members = append(out.Members, m)
	}
	out.ObservedAt = time.Now().UTC()
	return out, nil
}
