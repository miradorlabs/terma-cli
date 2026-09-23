// Package api is the typed HTTP client for the Terma API gateway.
//
// It owns two things no command should repeat: attaching the right credential
// (server key or CLI token, plus the per-request project header), and refreshing an
// expired CLI token transparently so a long-lived shell never sees a spurious 401.
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

	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
)

const (
	userAgentPrefix = "terma-cli/"

	projectHeader = "X-Mirador-Project"

	// maxPlainErrorLen bounds an error body that is not the gateway's JSON envelope.
	maxPlainErrorLen = 200
)

// APIError is a structured failure from the gateway. Commands print Message rather
// than a bare status code, since the gateway's messages name the fix.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
	RequestID  string
}

func (e *APIError) Error() string {
	msg := e.Message
	// The shared gateway's remedy names its original CLI. Keep the protocol header
	// unchanged, but give Terma users the command that binds their repository.
	if e.Code == "INVALID_ARGUMENT" && strings.Contains(msg, "missing X-Mirador-Project header") {
		msg = "no project selected — run `terma install` in this repository or pass --project"
	}
	switch {
	case msg == "" && e.StatusCode != 0:
		msg = fmt.Sprintf("request failed with status %d", e.StatusCode)
	case msg == "":
		msg = "request failed"
	case e.Code == "" && e.StatusCode != 0:
		// No envelope: the message is a proxy's body or the status text, and the status
		// is the only other thing known about the failure.
		msg = fmt.Sprintf("%s (status %d)", msg, e.StatusCode)
	}
	var detail []string
	if e.Code != "" {
		detail = append(detail, e.Code)
	}
	if e.RequestID != "" {
		detail = append(detail, "request_id="+e.RequestID)
	}
	if len(detail) == 0 {
		return msg
	}
	return fmt.Sprintf("%s (%s)", msg, strings.Join(detail, ", "))
}

// Unauthenticated reports whether the credential itself was rejected, as opposed to
// the request being malformed or the caller lacking access to a specific resource.
func (e *APIError) Unauthenticated() bool { return e.StatusCode == http.StatusUnauthorized }

// Client talks to the API gateway as the signed-in developer, or as a server key. It
// refreshes a credential it knows has expired before the request, so the common case is
// one round trip; a 401 it did not expect — the token revoked or rotated elsewhere —
// gets one refresh and one retry, and a second 401 is a real logout. The selected
// project goes with every data-plane call.
type Client struct {
	// baseURL is the data plane; authURL is the credential surface. They are separate
	// hosts, so every call has to say which one it means — see dataHost/authHost.
	baseURL string
	authURL string
	http    *http.Client
	version string

	// profile and credential are empty under a server key: TERMA_API_KEY has
	// nothing to refresh and nothing to persist.
	profile string
	apiKey  string

	mu         sync.Mutex
	credential *auth.Credential
	lastLogin  *LoginResult
	// tokenGen increments every time credential is replaced. A request records the
	// generation it authenticated with; the 401 retry then refreshes only if that
	// generation is still current, so a request that raced a refresh another goroutine
	// already did retries with the new token instead of redeeming the refresh token a
	// second time.
	tokenGen uint64
	// refreshMu serializes the token exchange so two goroutines never redeem the same
	// rotated refresh token — which the server's reuse detection would treat as theft
	// and revoke the whole session.
	refreshMu sync.Mutex

	projectID string
}

// Options is what New needs beyond the resolved config.
type Options struct {
	// Version is the running terma's, for the User-Agent.
	Version string
	// ProjectID scopes data-plane requests; empty means the config's selected project.
	ProjectID string
	// Timeout bounds each request. Zero means the default.
	Timeout time.Duration
	// Credential, when set, is used instead of the profile's active credential. This is
	// how a command tries a stored credential for another organization before making
	// it active: a refresh it triggers is persisted for that organization, and nothing
	// else changes until the caller decides.
	Credential *auth.Credential
}

// New builds a client for the resolved config. A server key short-circuits the
// credential path entirely — that is the CI story, where no browser exists.
func New(cfg *config.Config, opts Options) (*Client, error) {
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	c := &Client{
		baseURL:   strings.TrimRight(cfg.APIURL, "/"),
		authURL:   strings.TrimRight(cfg.AuthURL, "/"),
		http:      newHTTPClient(timeout),
		version:   opts.Version,
		profile:   cfg.ProfileName,
		apiKey:    cfg.APIKey,
		projectID: firstNonEmpty(opts.ProjectID, cfg.ProjectID),
	}
	if c.apiKey != "" {
		return c, nil
	}

	cred := opts.Credential
	if cred == nil {
		loaded, err := auth.LoadCredential(cfg.ProfileName)
		if err != nil {
			return nil, err
		}
		cred = loaded
	}
	// Refuse before the first request rather than letting the wrong environment's auth
	// host answer 401 — a message naming both hosts is the difference between "my login
	// broke" and "I am pointed at dev".
	if err := cred.CheckEnvironment(c.authURL); err != nil {
		return nil, err
	}
	c.credential = cred
	return c, nil
}

// NewAnonymous builds a client for the endpoints that mint credentials, which by
// definition cannot present one. Only the auth host is reachable from it.
func NewAnonymous(authURL, version string) *Client {
	return &Client{
		authURL: strings.TrimRight(authURL, "/"),
		http:    newHTTPClient(30 * time.Second),
		version: version,
	}
}

// newHTTPClient builds the shared transport with redirects refused. An API client that
// carries bearer and refresh tokens must never follow a redirect: on a 307/308 Go
// replays the request body — an authorization code, a PKCE verifier, or a refresh
// token — to the redirect target, and on a same-host https→http downgrade it would put
// the Authorization header on the wire in cleartext. The gateways never redirect a JSON
// API call, so a redirect here is a misconfiguration or an attack, not a normal path.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: refuseRedirects}
}

func refuseRedirects(req *http.Request, _ []*http.Request) error {
	return fmt.Errorf("refusing to follow redirect to %s: the Terma API does not redirect API requests", req.URL.Redacted())
}

// host selects which surface a call targets. Requests never guess from the path —
// the two hosts have overlapping path prefixes and a wrong guess would send a
// credential to the wrong service.
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

// Credential returns the in-memory credential, which may be newer than the one on
// disk if a refresh has happened this run.
func (c *Client) Credential() *auth.Credential {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.credential
}

// Get and Post address the data plane.
func (c *Client) Get(ctx context.Context, path string, query url.Values, out any) error {
	_, err := c.do(ctx, dataHost, http.MethodGet, path, query, nil, nil, out)
	return err
}

// Post sends body to a data-plane path and decodes the response into out.
func (c *Client) Post(ctx context.Context, path string, body, out any) error {
	_, err := c.do(ctx, dataHost, http.MethodPost, path, nil, nil, body, out)
	return err
}

// Meta carries the parts of a response the conditional-write endpoints make
// load-bearing: the ETag that authorizes the next write, and the status that
// distinguishes a create from a replace.
type Meta struct {
	StatusCode int
	ETag       string
}

// Created reports whether a PUT allocated a new resource rather than replacing one.
func (m *Meta) Created() bool { return m != nil && m.StatusCode == http.StatusCreated }

// IsNotFound reports a 404, which a caller may treat as "nothing there yet" rather
// than as a failure.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// IsPreconditionFailed reports a 412: the resource changed between the read that
// produced the ETag and the write that presented it.
func IsPreconditionFailed(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusPreconditionFailed
}

// GetWithMeta is Get plus the ETag, which a later Put or Delete must present.
func (c *Client) GetWithMeta(ctx context.Context, path string, query url.Values, out any) (*Meta, error) {
	return c.do(ctx, dataHost, http.MethodGet, path, query, nil, nil, out)
}

// Precondition is the conditional header a write carries. The gateway requires
// exactly one: a write with neither is 428 and a write with both is 400, so this is
// modelled as one choice rather than two independent fields.
type Precondition struct {
	// CreateOnly sends `If-None-Match: *` — succeed only if the slug is unused.
	CreateOnly bool
	// ReplaceETag sends `If-Match: <etag>` — succeed only against that exact revision.
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
		// Caught here rather than at the gateway so the message names the CLI's own
		// flags instead of returning a bare 428.
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

// AuthGet and AuthPost address the credential surface.
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
	// Refresh before the first attempt when the token is known-expired, so the common
	// case costs one round trip rather than a guaranteed 401 followed by a retry.
	if err := c.refreshIfNeeded(ctx); err != nil {
		return nil, err
	}

	// Record which token generation this attempt authenticated with, so the 401 path
	// below can tell "my token is genuinely stale" from "another goroutine already
	// refreshed while my request was in flight".
	gen := c.currentGen()
	resp, err := c.attempt(ctx, h, method, path, query, headers, body)
	if err != nil {
		return nil, err
	}

	// A 401 on a token we believed was live means it was revoked or rotated
	// elsewhere. One refresh-and-retry covers that; a second 401 is a real logout.
	if resp.StatusCode == http.StatusUnauthorized && c.canRefresh() {
		resp.Body.Close()
		if refreshErr := c.refreshIfCurrent(ctx, gen); refreshErr != nil {
			return nil, refreshErr
		}
		resp, err = c.attempt(ctx, h, method, path, query, headers, body)
		if err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()

	meta := &Meta{StatusCode: resp.StatusCode, ETag: resp.Header.Get("ETag")}
	return meta, decode(resp, out)
}

func (c *Client) attempt(ctx context.Context, h host, method, path string, query url.Values, headers http.Header, body any) (*http.Response, error) {
	req, err := c.newRequest(ctx, h, method, path, query, headers, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	return resp, nil
}

// newRequest builds a fully-authorized request without sending it, so a caller
// needing different transport settings — the SSE tail, which must not inherit the
// shared client's request timeout — can reuse the credential and header logic
// rather than reimplementing it.
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

	// Only the data plane resolves a project, and only under a CLI token: a server
	// key's grant already fixes it, and the auth surface is organization-scoped
	// throughout. Sending it anywhere else would imply a scope that does not exist.
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

func parseError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var envelope struct {
		Error struct {
			Code    string           `json:"code"`
			Message string           `json:"message"`
			Details []map[string]any `json:"details"`
		} `json:"error"`
	}
	apiErr := &APIError{StatusCode: resp.StatusCode}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Message != "" {
		apiErr.Code = envelope.Error.Code
		apiErr.Message = envelope.Error.Message
		for _, detail := range envelope.Error.Details {
			if id, ok := detail["request_id"].(string); ok {
				apiErr.RequestID = id
			}
		}
		return apiErr
	}
	// Not the gateway's envelope, so whatever sits in front of it answered: keep the
	// first line, bounded, rather than printing a proxy's whole HTML error page.
	apiErr.Message, _, _ = strings.Cut(strings.TrimSpace(string(body)), "\n")
	if len(apiErr.Message) > maxPlainErrorLen {
		apiErr.Message = apiErr.Message[:maxPlainErrorLen] + "…"
	}
	if apiErr.Message == "" || strings.HasPrefix(apiErr.Message, "<") {
		apiErr.Message = http.StatusText(resp.StatusCode)
	}
	return apiErr
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
