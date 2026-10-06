package repohooks

import (
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
)

// Wanted reports whether policy asks for terma's commit hooks in the repository at
// gitDir: a validated policy, not expired, with GitHooks on, that collects it. A policy
// no longer refreshed installs nothing, as it records nothing.
func Wanted(policy config.Policy, now time.Time, gitDir string) bool {
	if !policy.Validated() || policy.Expired(now) || !policy.GitHooks {
		return false
	}
	return policy.Admits(config.Repository{Origin: gitx.RepositoryFS(gitDir)})
}

// OwnHooksPath reports whether git reads this repository's hooks from a core.hooksPath,
// set for the repository (husky v9, lefthook) or in git's global config: git then never
// reads .git/hooks, so terma skips it rather than overriding that choice.
func OwnHooksPath(gitDir string) bool { return HooksPathScope(gitDir) != "" }

// Sync is what a claimed agent session does about its repository's commit hooks: where
// the policy asks for them and they are not there, it installs them. Nothing else ever
// changes them, so they never come and go with the policy. It runs inside a hook's
// budget, so it reads the filesystem and never git, and writes only when something is
// missing.
func Sync(stateDir, terma string, policy config.Policy, now time.Time, dir string) (changed bool, err error) {
	_, gitDir, ok := gitx.LocateFS(dir)
	if !ok || !Wanted(policy, now, gitDir) || OwnHooksPath(gitDir) {
		return false, nil
	}
	return Install(stateDir, terma, gitDir)
}
