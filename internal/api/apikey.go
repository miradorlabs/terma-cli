package api

import (
	"context"
	"errors"
)

// ServerKey is the non-secret half of a minted key — everything safe to print, store,
// or show in a table. The plaintext key is deliberately not a field here so it cannot
// be rendered by accident alongside the rest.
type ServerKey struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	KeyPrefix string `json:"key_prefix"`
	CreatedAt string `json:"created_at,omitempty"`
}

type createServerKeyRequest struct {
	ProjectID   string               `json:"project_id"`
	Name        string               `json:"name"`
	Description string               `json:"description,omitempty"`
	Permissions serverKeyPermissions `json:"permissions"`
}

type serverKeyPermissions struct {
	Read   bool `json:"read"`
	Write  bool `json:"write"`
	Ingest bool `json:"ingest"`
}

type createServerKeyResponse struct {
	Key       string    `json:"key"`
	ServerKey ServerKey `json:"server_key"`
}

// CreateServerKey returns an ingest-only server key (ter_srv_…) bound to one project.
//
// The key is ingest-only because everything this machine sends with it is telemetry.
// The server returns the project's active ingest-only key of the same name when one
// exists, so re-running install or connect after the local copy was lost gets that
// key back rather than minting another. The plaintext is returned as a separate value
// rather than a struct field, forcing every caller to decide what to do with it.
//
// This requires a user credential. A server key cannot mint another, so this is one of
// the few operations TERMA_API_KEY cannot perform; the error says so plainly rather
// than surfacing a bare 403.
func (c *Client) CreateServerKey(ctx context.Context, projectID, name, description string) (key string, meta ServerKey, err error) {
	if c.apiKey != "" {
		return "", ServerKey{}, errors.New(
			"minting a server key needs a user credential, and TERMA_API_KEY is set — unset it and run `terma login`")
	}
	if projectID == "" {
		return "", ServerKey{}, errors.New("a server key must name the project it is bound to")
	}
	if name == "" {
		return "", ServerKey{}, errors.New("a server key needs a name — it is the only handle for revoking it later")
	}

	var resp createServerKeyResponse
	if err := c.AuthPost(ctx, "/v1/api-keys/server", createServerKeyRequest{
		ProjectID:   projectID,
		Name:        name,
		Description: description,
		Permissions: serverKeyPermissions{Ingest: true},
	}, &resp); err != nil {
		return "", ServerKey{}, err
	}
	if resp.Key == "" {
		return "", ServerKey{}, errors.New("the server minted a key but returned no value for it — nothing was written")
	}
	return resp.Key, resp.ServerKey, nil
}
