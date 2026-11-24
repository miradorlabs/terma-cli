package hookrun

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"mvdan.cc/sh/v3/pattern"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// Classify classifies the checkouts paths reach (including symlinks and subdirs unless Alone)
// against r and the run's earlier locations. UnknownReach marks the run Mixed. Returns r or located.
func (e Env) Classify(ctx context.Context, r *Repo, sess session.Session, paths []string, extra map[string]any) *Repo {
	if !e.Routing.Usable() || !session.ValidID(sess.ID) {
		return r
	}
	var unknown runs.Why
	paths = slices.DeleteFunc(searchedBy(paths, e.Cwd, os.LookupEnv, time.Now().Add(maxShellTime)), func(p string) bool {
		if isUnknownReach(p) {
			_, cmd, _ := strings.Cut(p[len(UnknownReach):], "\x00")
			unknown = runs.Why{Reason: runs.UnknownReach, Where: cmd}
		}
		return p == "" || isUnknownReach(p)
	})
	if len(paths) == 0 && unknown.Reason == "" {
		return r
	}
	if r == nil {
		var err error
		if r, err = e.routeHere(ctx); err != nil {
			var locs []routing.Location
			for _, t := range e.reached(nil, paths) {
				if t.repo.dest != nil {
					locs = append(locs, t.repo.dest.loc)
				}
			}
			if len(locs) > 0 || unknown.Reason != "" {
				agent, _ := extra[semconv.TermaAgentIDKey].(string)
				e.judge(sess.ID, agent, claim.Claim{Tool: sess.Tool, PIDs: claimPIDs(), Parent: e.parent}, locs, false, unknown)
			}
			return nil
		}
	}
	e.classifyTouches(r, sess, extra, e.reached(r, paths), unknown)
	return r
}
func (e Env) reached(r *Repo, paths []string) (out []touched) {
	dirs := map[string]bool{}
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(e.Cwd, p)
		}
		p = filepath.Clean(p)
		at := []string{p}
		if target, err := filepath.EvalSymlinks(p); err == nil && target != p {
			at = append(at, target)
		}
		for _, dir := range at {
			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				dir = filepath.Dir(dir)
			}
			if !dirs[dir] {
				dirs[dir] = true
				if repo := e.checkoutOf(r, dir); repo != nil && repo != r && !slices.ContainsFunc(out, func(t touched) bool { return samePath(t.repo.Root, repo.Root) }) {
					out = append(out, touched{repo: repo})
				}
			}
		}
	}
	return out
}

// deviceRoots are the device and kernel filesystems, which are no one's workspace.
var deviceRoots = []string{"/dev", "/proc", "/sys"}

// machineRoots are the folders that are the machine's own, each as named and as its links
// resolve: the operating system's configuration and programs, and the scratch space
// (/tmp is /private/tmp on macOS, and $TMPDIR a folder of /var/folders there).
var machineRoots = sync.OnceValue(func() (roots []string) {
	for _, root := range []string{"/etc", "/bin", "/sbin", "/usr", "/lib", "/lib64", "/System", "/Library", "/tmp", "/var/tmp", os.TempDir()} {
		if root = filepath.Clean(root); filepath.IsAbs(root) && root != filepath.Dir(root) {
			roots = appendUnique(roots, root, resolved(root))
		}
	}
	return roots
})

// nobodys reports whether dir, a folder in no git checkout, is no one's workspace, so that
// reaching it crosses into no organization, personal included (checkoutOf): the
// filesystem's root folder itself, a device or kernel filesystem (2>/dev/null,
// /proc/cpuinfo), the operating system's own folders (/etc/hosts, /usr/bin), or the
// machine's scratch space (ls /tmp, a mktemp file), which every tool and every team's
// session on the machine shares. Any other folder outside git may hold the developer's own
// work, and stays theirs: so does a folder of their home wherever the home is, and a git
// checkout kept in scratch space is still its origin's, never asked about here.
func nobodys(dir string) bool {
	if dir == filepath.Dir(dir) || slices.ContainsFunc(deviceRoots, func(root string) bool { return under(dir, root) }) {
		return true
	}
	if home, err := os.UserHomeDir(); err == nil && home != filepath.Dir(home) && (under(dir, home) || under(dir, resolved(home))) {
		return false
	}
	return slices.ContainsFunc(machineRoots(), func(root string) bool { return under(dir, root) })
}

// under reports whether dir is root or a folder below it.
func under(dir, root string) bool {
	if root == "" || len(dir) < len(root) || dir[:len(root)] != root && (filepath.Separator != '\\' || !strings.EqualFold(dir[:len(root)], root)) {
		return false
	}
	return len(dir) == len(root) || os.IsPathSeparator(dir[len(root)])
}

const aloneMark = "\x00alone\x00"

// Alone marks paths that reach themselves alone (searchedEnv does not walk below them).
func Alone(paths ...string) []string {
	out := slices.Clone(paths)
	for i, p := range out {
		out[i] = pick(p != "" && !isUnknownReach(p) && !strings.HasPrefix(p, aloneMark), aloneMark+p, p)
	}
	return out
}

// Search returns dir and the base folder each glob names before its first wildcard.
func Search(dir string, globs ...string) []string { return searchEnv(dir, os.LookupEnv, globs...) }
func searchEnv(dir string, env func(string) (string, bool), globs ...string) []string {
	out := []string{dir}
	for _, g := range globs {
		if alts, ok := expandGlobBraces(globToSlash(g), 32); !ok {
			out = append(out, unknownReach(g))
		} else {
			for _, slashed := range alts {
				if strings.HasPrefix(slashed, "~") {
					home, ok := env("HOME")
					if (slashed != "~" && !strings.HasPrefix(slashed, "~/")) || !ok || home == "" || slashed == "~" {
						out = append(out, pick(slashed == "~" && ok && home != "", home, unknownReach(g)))
						continue
					}
					slashed = strings.TrimRight(filepath.ToSlash(home), "/") + slashed[1:]
				}
				if p := literalPrefix(slashed); p != "" {
					out = append(out, pick(!filepath.IsAbs(p), filepath.Join(dir, p), pick(!strings.HasPrefix(p, "//"), filepath.FromSlash(p), p)))
				}
			}
		}
	}
	return out
}
func globToSlash(g string) string {
	if filepath.Separator != '\\' || !strings.ContainsRune(g, '\\') {
		return g
	}
	b, hasSlash := []byte(g), strings.ContainsRune(g, '/')
	for i := range b {
		if b[i] == '\\' && (!hasSlash || i+1 >= len(b) || strings.IndexByte("*?[]{}", b[i+1]) < 0) {
			b[i] = '/'
		}
	}
	return string(b)
}
func expandGlobBraces(s string, limit int) ([]string, bool) {
	if open := strings.IndexByte(s, '{'); open < 0 || strings.Count(s, "{") > limit {
		return pick(open < 0, []string{s}, nil), open < 0
	} else if closeIdx := strings.IndexByte(s[open+1:], '}'); closeIdx < 0 {
		return []string{s}, !strings.ContainsRune(s[open:], '/')
	} else if body, parts, out := s[open+1:open+1+closeIdx], strings.Split(s[open+1:open+1+closeIdx], ","), []string(nil); strings.ContainsRune(body, '{') || len(parts) < 2 || len(parts) > limit {
		return nil, false
	} else {
		for _, p := range parts {
			sub, ok := expandGlobBraces(s[:open]+p+s[open+closeIdx+2:], limit-len(out))
			if !ok || len(out)+len(sub) > limit {
				return nil, false
			}
			out = append(out, sub...)
		}
		return out, true
	}
}
func literalPrefix(glob string) string {
	segs := strings.Split(glob, "/")
	n := slices.IndexFunc(segs, func(s string) bool { return pattern.HasMeta(s, 0) })
	if n == 1 && segs[0] == "" {
		return "/"
	}
	n = pick(n < 0, len(segs)-1, n)
	for i := range n {
		segs[i] = unescape(segs[i])
	}
	return strings.Join(segs[:n], "/")
}
func appendUnique(out []string, items ...string) []string {
	for _, p := range items {
		out = pick(slices.Contains(out, p), out, append(out, p))
	}
	return out
}
func searchedBy(paths []string, cwd string, env func(string) (string, bool), deadline time.Time) []string {
	sc, unknownDir, out := newScan(cwd, env, work{deadline: deadline}), "", make([]string, 0, len(paths))
	for _, p := range paths {
		dir, alone := strings.CutPrefix(p, aloneMark)
		dir = pick(remote(dir, cwd), unknownReach(dir), dir)
		if out = appendUnique(out, dir); alone || dir == "" || isUnknownReach(dir) || unknownDir != "" {
			continue
		}
		if abs, ok := sc.path(dir, cwd); ok && sc.spend(1) {
			if r, _, info, found := sc.walkPath(abs, true); found && info.IsDir() {
				sc.below(r, maxBelowDepth+2)
			}
		}
		unknownDir = pick(sc.unknown, dir, unknownDir)
	}
	out = appendUnique(out, sc.out...)
	return pick(unknownDir != "", append(out, unknownReach(unknownDir)), out)
}
