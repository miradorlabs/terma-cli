package api

import (
	"context"
	"errors"
)

// ServerKey is the non-secret half of a minted key; the plaintext is not a field, so it
// cannot be rendered by accident.
type ServerKey struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	KeyPrefix string `json:"key_prefix"`
	CreatedAt string `json:"created_at,omitempty"`
}

type createServerKeyRequest struct {
	ProjectID   string `json:"project_id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type createServerKeyResponse struct {
	Key       string    `json:"key"`
	ServerKey ServerKey `json:"server_key"`
}

// CreateServerKey mints a server key bound to one project and returns its plaintext,
// which nothing can produce again; it needs a user credential, not TERMA_API_KEY.
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
	}, &resp); err != nil {
		return "", ServerKey{}, err
	}
	if resp.Key == "" {
		return "", ServerKey{}, errors.New("the server minted a key but returned no value for it — nothing was written")
	}
	return resp.Key, resp.ServerKey, nil
}
