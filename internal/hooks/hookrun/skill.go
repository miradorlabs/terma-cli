package hookrun

import (
	"encoding/json"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// SkillDescriptionMaxChars bounds a skill's description, in Unicode code points: a longer one
// is left out, never cut, since a cut description reads as the skill's own.
const SkillDescriptionMaxChars = 2048

// skillStateCap bounds how many skills one session's state remembers; a session that uses
// more may report the earliest again.
const skillStateCap = 1024

// skillLockWait is how long a hook waits for the session's skill state: two hooks of one
// session can run at once (parallel reads fire parallel tool hooks), and the second must
// still report what only it saw.
const skillLockWait = time.Second

// Skill is what a session used: a skill the agent names, what it says it does and where it
// is held. Every field but Name may be empty.
type Skill struct {
	Name, Description, Origin, Plugin, Marketplace string
	// RepositoryURL is the repository the skill's file is held in, and Path the file's place
	// in it: a path never goes without a URL, and a URL goes alone for a plugin whose
	// marketplace repository is known but whose file's place in it is not.
	RepositoryURL, Path string
}

// skillState is the skills a session already reported: hashes of their names, never the names.
type skillState struct {
	Reported []string `json:"reported"`
}

// SkillsCollected reports whether a skill's definition may leave: it is content of both
// kinds, a description written for the model and a tool's file, so only a policy that
// withholds nothing sends it.
func (e Env) SkillsCollected() bool {
	prompts, toolContent := e.Policy.Content()
	return prompts && toolContent
}

// SpoolSkills spools a terma.skill.definition for each skill the session has not reported
// yet, keeping which it has under stateDir. attrs are the agent's own (its name, where the
// skill was read). It reports false when a new skill could not be spooled, so a caller holds
// its cursor back; true otherwise, also when the policy withholds and nothing is spooled.
//
// Accepted risk: a definition is queued under r, the checkout its hook ran in, though a
// session resumed from another collected checkout may have used the skill there; if the
// policy drops that other checkout before the queue is flushed, delivery, which judges r,
// still sends the definition (a link to the dropped checkout is removed). Upgrade path: queue
// each definition under the checkout it was used in.
//
// Accepted risk: like a session's title and replies, a definition goes in the lane of the hook
// that reads the use, not the session's owner, so after a sign-in switch mid-session a use
// read late leaves under the new sign-in; session ownership guards file attribution only.
// Upgrade path: gate every session-content event on the owner together, not this one alone.
func SpoolSkills(e Env, r *Repo, stateDir, sessionID string, attrs map[string]any, skills []Skill) bool {
	if !e.SkillsCollected() || len(skills) == 0 {
		return true
	}
	if e.Spool == nil || !session.ValidID(sessionID) {
		return false
	}
	dir := filepath.Join(e.StateDir, stateDir)
	path := filepath.Join(dir, EvidenceID(sessionID)+".json")
	_, statErr := os.Lstat(path)
	ok := false
	err := flock.Locked(path, skillLockWait, func() error {
		var st skillState
		if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &st) != nil {
			st = skillState{}
		}
		ok = true
		changed := false
		lost := map[string]bool{}
		for _, s := range skills {
			one := skillAttrs(s)
			hash := EvidenceID(s.Name)
			if one == nil || slices.Contains(st.Reported, hash) || lost[hash] {
				continue
			}
			// With no lane (signed out, or a sign-in that can't be read) the use is lost and
			// counted, once per skill however often the batch names it, and the skill stays
			// unreported: a later use under a sign-in is a new collection, sent then.
			if lost[hash] = e.lost(r, 1); lost[hash] {
				continue
			}
			out := maps.Clone(attrs)
			maps.Copy(out, one)
			if !e.EmitFor(r, spool.Event{Name: semconv.TermaSkillDefinitionEvent, SessionID: sessionID, Attrs: out}) {
				ok = false
				break
			}
			st.Reported = append(st.Reported, hash)
			changed = true
		}
		if !changed {
			return nil
		}
		st.Reported = st.Reported[max(0, len(st.Reported)-skillStateCap):]
		b, err := json.Marshal(st)
		if err == nil {
			err = WriteState(path, b)
		}
		return err
	})
	if err != nil {
		e.Logf("skills: %v", err)
	}
	if os.IsNotExist(statErr) {
		PruneState(dir, e.Time().Add(-spool.MaxAge))
	}
	return ok
}

// skillOrigins are the values of terma.skill.origin.
var skillOrigins = []string{
	semconv.TermaSkillOriginPersonal, semconv.TermaSkillOriginRepository, semconv.TermaSkillOriginPlugin,
	semconv.TermaSkillOriginBuiltin, semconv.TermaSkillOriginManaged,
}

// skillAttrs is what of s may leave, nil when its name cannot: the description only whole,
// the origin only from the registry's list, and where the file is held only as a shareable
// URL and path together, or for a plugin with no path the URL alone.
func skillAttrs(s Skill) map[string]any {
	if s.Name == "" || len(s.Name) > maxAttrLen || !utf8.ValidString(s.Name) {
		return nil
	}
	out := map[string]any{semconv.TermaSkillNameKey: s.Name}
	if d := strings.TrimSpace(s.Description); d != "" && utf8.ValidString(d) && utf8.RuneCountInString(d) <= SkillDescriptionMaxChars {
		out[semconv.TermaSkillDescriptionKey] = d
	}
	if slices.Contains(skillOrigins, s.Origin) {
		out[semconv.TermaSkillOriginKey] = s.Origin
	}
	for k, v := range map[string]string{semconv.TermaSkillPluginNameKey: s.Plugin, semconv.TermaSkillMarketplaceNameKey: s.Marketplace} {
		if utf8.ValidString(v) {
			BoundedAttr(out, k, v)
		}
	}
	if shareableSkillURL(s.RepositoryURL) {
		switch {
		case shareableSkillPath(s.Path):
			out[semconv.TermaSkillRepositoryURLKey], out[semconv.TermaSkillPathKey] = s.RepositoryURL, s.Path
		case s.Path == "" && s.Origin == semconv.TermaSkillOriginPlugin:
			out[semconv.TermaSkillRepositoryURLKey] = s.RepositoryURL
		}
	}
	return out
}

// shareableSkillURL reports whether a skill's repository may leave: an https URL with no
// credentials.
func shareableSkillURL(rawURL string) bool {
	if rawURL == "" || len(rawURL) > maxAttrLen || !utf8.ValidString(rawURL) {
		return false
	}
	u, err := url.Parse(rawURL)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Opaque == "" && u.RawQuery == "" && u.Fragment == ""
}

// shareableSkillPath reports whether a skill file's place in its repository may leave: a
// relative path that cannot name a file on this machine.
func shareableSkillPath(path string) bool {
	if path == "" || len(path) > maxAttrLen || !utf8.ValidString(path) {
		return false
	}
	if strings.HasPrefix(path, "~") || strings.Contains(path, `\`) || len(path) >= 2 && path[1] == ':' {
		return false
	}
	for seg := range strings.SplitSeq(path, "/") {
		if seg == "" || seg == "." || seg == ".." { // a leading "/" is an empty first segment
			return false
		}
	}
	return true
}

// SkillLink is where the skill file at file is held, when that may leave: the https URL of
// the checkout its symlink-resolved path lies in, and the path relative to it, with forward
// slashes. Both are "" unless the checkout's origin is a repository the team's policy
// collects. It reads git's files and runs no git.
//
// Accepted risk: the file may be untracked or ignored in that checkout, so the link can lead
// nowhere. Upgrade path: ask git whether the path is tracked, off the hook's time budget.
func (e Env) SkillLink(file string) (repositoryURL, path string) {
	// LocateFS takes GIT_DIR as the checkout of any directory, which would misplace the file.
	if os.Getenv("GIT_DIR") != "" {
		return "", ""
	}
	resolved, err := filepath.EvalSymlinks(file)
	if err != nil {
		return "", ""
	}
	if st, err := os.Stat(resolved); err != nil || !st.Mode().IsRegular() {
		return "", ""
	}
	root, gitDir, ok := gitx.LocateFS(filepath.Dir(resolved))
	if !ok {
		return "", ""
	}
	id, _ := gitx.OriginFS(gitDir)
	if id == "" || !e.Policy.Admits(config.Repository{Origin: id}) {
		return "", ""
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ""
	}
	host, repoPath, _ := strings.Cut(id, "/")
	if host == "" || repoPath == "" || strings.Contains(host, ":") {
		return "", ""
	}
	return (&url.URL{Scheme: "https", Host: host, Path: "/" + repoPath}).String(), filepath.ToSlash(rel)
}

// PathBelow is path relative to root joined with elem, when path lies below it. path is
// judged as written, before its symlinks are resolved, since a skill's origin is where the
// agent found it; root is compared both as given and symlink-resolved (/tmp is /private/tmp
// on macOS).
func PathBelow(path, root string, elem ...string) (string, bool) {
	if root == "" {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		resolved = root
	}
	for _, base := range []string{root, resolved} {
		rel, err := filepath.Rel(filepath.Join(append([]string{base}, elem...)...), path)
		if err == nil && rel != "." && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return rel, true
		}
	}
	return "", false
}
