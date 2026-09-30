package routing

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/project"
)

func policyPath(team string) (string, error) {
	if !project.ValidID(team) {
		return "", errors.New("invalid policy team ID")
	}
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "policies", team+".json"), nil
}

// LoadPolicy is local only. Hooks never need a credential or a network request.
func LoadPolicy(team string) (config.Policy, bool, error) {
	path, err := policyPath(team)
	if err != nil {
		return config.Policy{}, false, err
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return config.Policy{}, false, nil
	}
	if err != nil {
		return config.Policy{}, false, err
	}
	var p config.Policy
	if err := json.Unmarshal(b, &p); err != nil {
		return config.Policy{}, false, err
	}
	if p.TeamID != team || p.FetchedAt.IsZero() || p.Mode != config.ModeRepo && p.Mode != config.ModeGlobal {
		return config.Policy{}, false, errors.New("invalid cached collection policy")
	}
	return p, true, nil
}

// SavePolicy atomically publishes a validated team's policy. Delayed responses
// cannot roll a team's revision back; another team's revision is independent.
func SavePolicy(p config.Policy) error {
	path, err := policyPath(p.TeamID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	unlock, err := flock.Lock(ctx, path+".lock")
	if err != nil {
		return err
	}
	defer unlock()
	prev, ok, _ := LoadPolicy(p.TeamID)
	if ok && prev.OrganizationID == p.OrganizationID && prev.AuthURL == p.AuthURL && prev.Revision > p.Revision {
		return errors.New("collection policy revision moved backwards")
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(path, b, 0o600)
}

// EffectivePolicy never borrows another team's capture grant. Unscoped policies
// support legacy profiles and explicit offline fixtures.
func EffectivePolicy(fallback config.Policy, team string) config.Policy {
	p, ok, err := LoadPolicy(team)
	if err == nil && ok && p.AppliesTo(fallback.OrganizationID, fallback.AuthURL) {
		return p
	}
	if err == nil && !ok && (fallback.TeamID == "" || fallback.TeamID == team) {
		return fallback
	}
	return config.Policy{Mode: config.ModeRepo, Signals: []string{}}
}
