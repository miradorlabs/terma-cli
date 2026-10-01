package routing

import (
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

// LoadPolicy reads a team's cached policy from disk; hooks never need a credential.
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

// SavePolicy writes a team's policy, refusing one whose revision moved backwards.
func SavePolicy(p config.Policy) error {
	path, err := policyPath(p.TeamID)
	if err != nil {
		return err
	}
	return flock.Locked(path, 5*time.Second, func() error {
		prev, ok, _ := LoadPolicy(p.TeamID)
		if ok && prev.OrganizationID == p.OrganizationID && prev.AuthURL == p.AuthURL && prev.Revision > p.Revision {
			return errors.New("collection policy revision moved backwards")
		}
		b, err := json.Marshal(p)
		if err != nil {
			return err
		}
		return config.WriteFileAtomic(path, b, 0o600)
	})
}

// EffectivePolicy is team's cached policy, else an unscoped or same-team fallback, else
// collect nothing; it never borrows another team's grant.
func EffectivePolicy(fallback config.Policy, team string) config.Policy {
	p, ok, err := LoadPolicy(team)
	if err == nil && ok && p.AppliesTo(fallback.OrganizationID, fallback.AuthURL) {
		return p
	}
	if err == nil && !ok && (fallback.TeamID == "" || fallback.TeamID == team) {
		return fallback
	}
	return config.NoPolicy("", "")
}

// StorePolicy records a freshly fetched policy: the team's cache, and the profile's
// machine-wide coverage when it is the profile's team. It serializes with login changes
// and refuses a policy fetched under an organization the profile has since left.
func StorePolicy(cfg *config.Config, pol *config.Policy) error {
	var rejected error
	err := config.UpdateProfile(cfg.ProfileName, func(p *config.Profile) {
		if p.OrganizationID == "" && cfg.OrganizationID != "" {
			p.SelectOrganization(cfg.OrganizationID, "")
		}
		if p.OrganizationID != cfg.OrganizationID {
			rejected = errors.New("organization changed while fetching collection policy")
			return
		}
		if pol.TeamID != "" {
			if err := SavePolicy(*pol); err != nil {
				rejected = err
				return
			}
		}
		if p.Policy == nil || p.Policy.TeamID == pol.TeamID || cfg.Policy.TeamID == pol.TeamID {
			p.Policy = pol
		}
	})
	if err != nil {
		return err
	}
	return rejected
}

// ValidatedPolicy is team's last validated policy for cfg's organization and
// environment; a corrupt cache grants nothing.
func ValidatedPolicy(cfg *config.Config, team string) (config.Policy, bool) {
	cached, ok, err := LoadPolicy(team)
	if err != nil {
		return config.Policy{}, false
	}
	if ok && !cached.AppliesTo(cfg.OrganizationID, cfg.AuthURL) {
		ok = false
	}
	if !ok && cfg.Policy.TeamID == team && cfg.Policy.AppliesTo(cfg.OrganizationID, cfg.AuthURL) && !cfg.Policy.FetchedAt.IsZero() {
		cached, ok = cfg.Policy, true
	}
	return cached, ok
}
