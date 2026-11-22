// Package api is the typed HTTP client for the Terma API gateway. It attaches the
// credential and project header and refreshes an expired CLI token transparently.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
)

const (
	userAgentPrefix = "terma-cli/"

	projectHeader = "X-Mirador-Project"

	maxPlainErrorLen = 200
)

// Client talks to the API gateway as the signed-in developer or a server key; an
// unexpected 401 gets one refresh and one retry, and a second is a real logout.
type Client struct {
	baseURL string
	authURL string
	http    *http.Client
	version string

	// dir and profile say where a refreshed credential is persisted.
	dir     string
	profile string
	apiKey  string
	// installationID is sent with a sign-in's code exchange.
	installationID string

	mu         sync.Mutex
	credential *auth.Credential
	// tokenGen counts credential replacements, so a 401 that raced another goroutine's
	// refresh retries with its token instead of redeeming the refresh token twice.
	tokenGen uint64
	// unsaved holds the refresh tokens this client spent since its last successful save.
	// A store that still holds one of them is behind this client.
	unsaved []string
	// refused is the refresh token the auth gateway last refused, never presented again.
	refused string
	// refreshing is a one-slot semaphore for this process's token exchanges. The
	// credential lock serializes them across processes.
	refreshing chan struct{}

	projectID string
}

// Options is what New needs beyond the resolved config.
type Options struct {
	Version   string
	ProjectID string
	Timeout   time.Duration
	// Credential, when set, replaces the profile's stored one.
	Credential *auth.Credential
}

// New builds a client for the resolved config; a server key skips the credential path.
func New(cfg *config.Config, opts Options) (*Client, error) {
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	c := &Client{
		baseURL:    strings.TrimRight(cfg.APIURL, "/"),
		authURL:    strings.TrimRight(cfg.AuthURL, "/"),
		http:       newHTTPClient(timeout),
		version:    opts.Version,
		dir:        cfg.Dir,
		profile:    cfg.ProfileName,
		apiKey:     cfg.APIKey,
		projectID:  firstNonEmpty(opts.ProjectID, cfg.ProjectID),
		refreshing: make(chan struct{}, 1),
	}
	if c.apiKey != "" {
		return c, nil
	}

	cred := opts.Credential
	if cred == nil {
		loaded, err := auth.LoadCredential(cfg.Dir, cfg.ProfileName)
		if err != nil {
			return nil, err
		}
		cred = loaded
	}
	// Refused here, naming both hosts, rather than as the wrong auth host's bare 401.
	if err := cred.CheckEnvironment(c.authURL); err != nil {
		return nil, err
	}
	c.credential = cred
	return c, nil
}

// NewAnonymous builds a credential-less client for the auth host's public endpoints.
func NewAnonymous(authURL, version string) *Client {
	return &Client{
		authURL:    strings.TrimRight(authURL, "/"),
		http:       newHTTPClient(30 * time.Second),
		version:    version,
		refreshing: make(chan struct{}, 1),
	}
}

// NewSignIn builds the credential-less client a sign-in exchanges its code with, for the
// installation installationID (config.InstallationID).
func NewSignIn(authURL, version, installationID string) *Client {
	c := NewAnonymous(authURL, version)
	c.installationID = installationID
	return c
}

// newHTTPClient refuses redirects: on a 307/308 Go replays the body (a code, verifier
// or refresh token) to the target, and an https→http downgrade leaks the bearer token.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: refuseRedirects}
}

func refuseRedirects(req *http.Request, _ []*http.Request) error {
	return fmt.Errorf("refusing to follow redirect to %s: the Terma API does not redirect API requests", req.URL.Redacted())
}

// host selects which surface a call targets; the hosts share path prefixes, so it is
// never guessed from the path.
type host int

const (
	dataHost host = iota
	authHost
)

func (c *Client) baseFor(h host) string {
	if h == authHost {
		return c.authURL
	}
	return c.baseURL
}

// ProjectID is the project this client's data-plane requests are scoped to.
func (c *Client) ProjectID() string { return c.projectID }

// Credential returns the in-memory credential, which a refresh may have made newer than disk's.
func (c *Client) Credential() *auth.Credential {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.credential
}

// Get reads a data-plane path into out.
func (c *Client) Get(ctx context.Context, path string, query url.Values, out any) error {
	_, err := c.do(ctx, dataHost, http.MethodGet, path, query, nil, nil, out)
	return err
}

// Post sends body to a data-plane path and decodes the response into out.
func (c *Client) Post(ctx context.Context, path string, body, out any) error {
	_, err := c.do(ctx, dataHost, http.MethodPost, path, nil, nil, body, out)
	return err
}

// Meta carries a response's status and the ETag that authorizes the next write.
type Meta struct {
	StatusCode int
	ETag       string
}

// Created reports whether a PUT allocated a new resource rather than replacing one.
func (m *Meta) Created() bool { return m != nil && m.StatusCode == http.StatusCreated }

// IsNotFound reports whether err is a 404.
func IsNotFound(err error) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// GetWithMeta is Get plus the ETag, which a later Put or Delete must present.
func (c *Client) GetWithMeta(ctx context.Context, path string, query url.Values, out any) (*Meta, error) {
	return c.do(ctx, dataHost, http.MethodGet, path, query, nil, nil, out)
}

// Precondition is the one conditional header a write carries: the gateway answers
// neither with 428 and both with 400.
type Precondition struct {
	// CreateOnly sends `If-None-Match: *`.
	CreateOnly bool
	// ReplaceETag sends `If-Match: <etag>`.
	ReplaceETag string
}

func (p Precondition) header() (string, string, error) {
	switch {
	case p.CreateOnly && p.ReplaceETag != "":
		return "", "", errors.New("a write is either a create or a replace, not both")
	case p.CreateOnly:
		return "If-None-Match", "*", nil
	case p.ReplaceETag != "":
		return "If-Match", p.ReplaceETag, nil
	default:
		// Caught here so the message names the CLI's flags, not a bare 428.
		return "", "", errors.New("a write needs a precondition: --create to add, or a prior read's etag to replace")
	}
}

// Put performs the conditional full-document write the resource endpoints require.
func (c *Client) Put(ctx context.Context, path string, pre Precondition, body, out any) (*Meta, error) {
	name, value, err := pre.header()
	if err != nil {
		return nil, err
	}
	return c.do(ctx, dataHost, http.MethodPut, path, nil, http.Header{name: []string{value}}, body, out)
}

// Delete removes a resource, and only the revision the ETag names.
func (c *Client) Delete(ctx context.Context, path, etag string) error {
	if etag == "" {
		return errors.New("delete needs the etag of the revision to remove")
	}
	_, err := c.do(ctx, dataHost, http.MethodDelete, path, nil, http.Header{"If-Match": []string{etag}}, nil, nil)
	return err
}

// AuthGet reads a credential-surface path into out.
func (c *Client) AuthGet(ctx context.Context, path string, query url.Values, out any) error {
	_, err := c.do(ctx, authHost, http.MethodGet, path, query, nil, nil, out)
	return err
}

// AuthPost is Post against the credential surface, which is a different host.
func (c *Client) AuthPost(ctx context.Context, path string, body, out any) error {
	_, err := c.do(ctx, authHost, http.MethodPost, path, nil, nil, body, out)
	return err
}

func (c *Client) do(ctx context.Context, h host, method, path string, query url.Values, headers http.Header, body, out any) (*Meta, error) {
	// A retired sign-in is never presented again: only signing in anew sends again.
	if c.retired() {
		return nil, errRetired
	}
	if err := c.refreshIfNeeded(ctx); err != nil {
		return nil, err
	}

	gen := c.currentGen()
	resp, err := c.attempt(ctx, h, method, path, query, headers, body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized && c.canRefresh() {
		resp.Body.Close()
		// The refused token is redeemed past unless another process already has.
		refused := func(held, current *auth.Credential) bool {
			return current.AccessToken == held.AccessToken || current.Expired()
		}
		if refreshErr := c.refreshIfCurrent(ctx, gen, refused); refreshErr != nil {
			return nil, refreshErr
		}
		resp, err = c.attempt(ctx, h, method, path, query, headers, body)
		if err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()

	meta := &Meta{StatusCode: resp.StatusCode, ETag: resp.Header.Get("ETag")}
	err = decode(resp, out)
	if apiErr, ok := errors.AsType[*Error](err); ok && apiErr.SignInRetired() {
		c.markRetired()
		return meta, fmt.Errorf("%w: %w", errRetired, err)
	}
	return meta, err
}

// errRetired is a request a server refused, or this client would not send, because the
// sign-in is retired. It reads as signed out too, so a sign-in replaces it.
var errRetired = fmt.Errorf("%w: %w", auth.ErrSignInRetired, auth.ErrNotLoggedIn)

// retired reports whether the sign-in this client holds is retired.
func (c *Client) retired() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.credential != nil && !c.credential.RetiredAt.IsZero()
}

// MarkRetired is markRetired, for a refusal another sender read: an ingest's.
func (c *Client) MarkRetired() { c.markRetired() }

// markRetired records, on the pair this client holds and on the stored sign-in while it is
// still that one, that a server refused it as retired.
func (c *Client) markRetired() {
	cred := c.retireHeld()
	if c.profile != "" && cred != nil && cred.SessionID != "" {
		_ = auth.MarkRetired(c.dir, c.profile, cred.SessionID)
	}
}

// retireHeld marks the pair this client holds retired, and returns it as it was.
func (c *Client) retireHeld() *auth.Credential {
	c.mu.Lock()
	defer c.mu.Unlock()
	cred := c.credential
	if cred != nil && cred.RetiredAt.IsZero() {
		marked := *cred
		marked.RetiredAt = time.Now().UTC()
		c.credential = &marked
	}
	return cred
}

func (c *Client) attempt(ctx context.Context, h host, method, path string, query url.Values, headers http.Header, body any) (*http.Response, error) {
	req, err := c.newRequest(ctx, h, method, path, query, headers, body)
	if err != nil {
		return nil, err
	}
	return c.send(req, method, path)
}

// attemptAs is attempt for a request whose body is its credential (a code or a refresh
// token): it carries the client's own only when withCredential asks.
func (c *Client) attemptAs(ctx context.Context, h host, method, path string, body any, withCredential bool) (*http.Response, error) {
	req, err := c.newRequest(ctx, h, method, path, nil, nil, body)
	if err != nil {
		return nil, err
	}
	if !withCredential {
		req.Header.Del("Authorization")
	}
	return c.send(req, method, path)
}

func (c *Client) send(req *http.Request, method, path string) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	return resp, nil
}

// newRequest builds an authorized request without sending it, for the SSE tail, which
// must not inherit the shared client's timeout.
func (c *Client) newRequest(ctx context.Context, h host, method, path string, query url.Values, headers http.Header, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request body: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	target := c.baseFor(h) + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgentPrefix+c.version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, values := range headers {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}

	c.mu.Lock()
	switch {
	case c.apiKey != "":
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	case c.credential != nil:
		req.Header.Set("Authorization", "Bearer "+c.credential.AccessToken)
	}
	c.mu.Unlock()

	// Only the data plane under a CLI token takes a project: a server key's grant fixes
	// it, and the auth surface is organization-scoped.
	if h == dataHost && c.apiKey == "" && c.projectID != "" {
		req.Header.Set(projectHeader, c.projectID)
	}

	return req, nil
}

func decode(resp *http.Response, out any) error {
	if resp.StatusCode >= 400 {
		return parseError(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
