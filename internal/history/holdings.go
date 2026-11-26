package history

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

const (
	// HoldingsChunkSize bounds the number of session IDs queried per GET /v1/ai/sessions call.
	HoldingsChunkSize = 50
	// HoldingsMaxFilterBytes keeps each AIP-160 filter expression safely below the gateway's
	// 4,096-byte filter limit.
	HoldingsMaxFilterBytes = 3800
	holdingsPerPage        = 200
	holdingsMaxPages       = 500
	aiSessionsPath         = "/v1/ai/sessions"
)

// HeldSet records which sessions a destination team already holds on the server.
type HeldSet struct {
	bySourceID map[string]bool
	byID       map[string]bool
}

// NewHeldSet constructs an empty HeldSet.
func NewHeldSet() HeldSet {
	return HeldSet{
		bySourceID: map[string]bool{},
		byID:       map[string]bool{},
	}
}

// Add marks (sourceSystem, sessionID) as already held by the destination team.
func (h *HeldSet) Add(sourceSystem, sessionID string) {
	if sessionID == "" {
		return
	}
	if h.bySourceID == nil {
		h.bySourceID = map[string]bool{}
	}
	if h.byID == nil {
		h.byID = map[string]bool{}
	}
	if sourceSystem != "" {
		h.bySourceID[SessionKey(sourceSystem, sessionID)] = true
	} else {
		h.byID[sessionID] = true
	}
}

// Contains reports whether the destination team already holds (sourceSystem, sessionID).
func (h HeldSet) Contains(sourceSystem, sessionID string) bool {
	if h.bySourceID[SessionKey(sourceSystem, sessionID)] {
		return true
	}
	return h.byID[sessionID]
}

// HoldingsChecker queries the API gateway for sessions a destination team already holds.
type HoldingsChecker interface {
	Check(ctx context.Context, team routing.Team, sessionIDs []string) (HeldSet, error)
}

// APIHoldingsChecker checks server holdings through terma's typed REST API client.
type APIHoldingsChecker struct {
	// ClientFor returns an authenticated API client scoped to teamID.
	ClientFor func(teamID string) (*api.Client, error)
	// ChunkSize overrides HoldingsChunkSize when > 0.
	ChunkSize int
	// MaxFilterBytes overrides HoldingsMaxFilterBytes when > 0.
	MaxFilterBytes int
}

type listSessionsResponse struct {
	Sessions *[]struct {
		SourceSystem string `json:"source_system"`
		SessionID    string `json:"session_id"`
		Historical   bool   `json:"historical"`
	} `json:"sessions"`
	Pagination *struct {
		Page       int `json:"page"`
		PerPage    int `json:"per_page"`
		Total      int `json:"total"`
		TotalPages int `json:"total_pages"`
	} `json:"pagination"`
	ProjectID string `json:"project_id"`
}

// Check queries GET /v1/ai/sessions for team in chunks of up to 50 session IDs (capped
// below the 4,096-byte filter limit). Any error fails closed so callers never upload
// sessions without confirming holdings.
func (c *APIHoldingsChecker) Check(ctx context.Context, team routing.Team, sessionIDs []string) (HeldSet, error) {
	held := NewHeldSet()
	if len(sessionIDs) == 0 {
		return held, nil
	}
	if c == nil || c.ClientFor == nil {
		return HeldSet{}, fmt.Errorf("holdings checker is not configured")
	}
	chunkSize := c.ChunkSize
	if chunkSize <= 0 {
		chunkSize = HoldingsChunkSize
	}
	maxBytes := c.MaxFilterBytes
	if maxBytes <= 0 {
		maxBytes = HoldingsMaxFilterBytes
	}
	filters, err := BuildSessionIDFilterChunks(sessionIDs, chunkSize, maxBytes)
	if err != nil {
		return HeldSet{}, err
	}
	client, err := c.ClientFor(team.TeamID)
	if err != nil {
		return HeldSet{}, err
	}
	for _, filter := range filters {
		for page := 1; page <= holdingsMaxPages; page++ {
			if err := ctx.Err(); err != nil {
				return HeldSet{}, err
			}
			q := url.Values{
				"filter":   {filter},
				"per_page": {strconv.Itoa(holdingsPerPage)},
				"page":     {strconv.Itoa(page)},
			}
			var resp listSessionsResponse
			if err := client.Get(ctx, aiSessionsPath, q, &resp); err != nil {
				return HeldSet{}, err
			}
			if resp.Sessions == nil || resp.Pagination == nil || resp.Pagination.Page < 1 || resp.Pagination.PerPage < 1 {
				return HeldSet{}, fmt.Errorf("unexpected holdings response envelope from %s", aiSessionsPath)
			}
			if resp.ProjectID == "" || resp.ProjectID != team.TeamID {
				return HeldSet{}, fmt.Errorf("holdings response project_id %q does not match requested team %q", resp.ProjectID, team.TeamID)
			}
			if resp.Pagination.Page != page {
				return HeldSet{}, fmt.Errorf("holdings response page %d does not match requested page %d", resp.Pagination.Page, page)
			}
			for _, s := range *resp.Sessions {
				held.Add(s.SourceSystem, s.SessionID)
			}
			if resp.Pagination.TotalPages <= page {
				break
			}
			// An empty page that is not the last would end the walk early and report
			// sessions on later pages as not held, so fail closed instead.
			if len(*resp.Sessions) == 0 {
				return HeldSet{}, fmt.Errorf("holdings page %d is empty but total_pages=%d", page, resp.Pagination.TotalPages)
			}
			if page == holdingsMaxPages {
				return HeldSet{}, fmt.Errorf("holdings pagination exceeded %d pages (total_pages=%d)", holdingsMaxPages, resp.Pagination.TotalPages)
			}
		}
	}
	return held, nil
}

// BuildSessionIDFilterChunks splits unique session IDs into AIP-160 filter expressions of
// the form `session_id="a" OR session_id="b"`, keeping each chunk within both maxIDs and
// maxBytes (safely under the API gateway's 4,096-byte filter cap).
func BuildSessionIDFilterChunks(ids []string, maxIDs, maxBytes int) ([]string, error) {
	if maxIDs <= 0 {
		maxIDs = HoldingsChunkSize
	}
	if maxBytes <= 0 {
		maxBytes = HoldingsMaxFilterBytes
	}
	unique := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
	}
	slices.Sort(unique)

	var chunks []string
	var cur strings.Builder
	curCount := 0
	flush := func() {
		if curCount > 0 {
			chunks = append(chunks, cur.String())
			cur.Reset()
			curCount = 0
		}
	}
	for _, id := range unique {
		clause := `session_id="` + escapeFilterString(id) + `"`
		if len(clause) > maxBytes {
			return nil, fmt.Errorf("session_id %q exceeds filter byte limit (%d > %d)", id, len(clause), maxBytes)
		}
		addLen := len(clause)
		if curCount > 0 {
			addLen += len(" OR ")
		}
		if curCount >= maxIDs || (curCount > 0 && cur.Len()+addLen > maxBytes) {
			flush()
		}
		if curCount > 0 {
			cur.WriteString(" OR ")
		}
		cur.WriteString(clause)
		curCount++
	}
	flush()
	return chunks, nil
}

func escapeFilterString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`)
	return r.Replace(s)
}
