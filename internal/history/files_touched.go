package history

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// normalizeFilesTouched resolves each edited file path on a terma.files.touched record to
// its own git checkout (handling sibling linked worktrees while dropping nested checkouts of
// other repositories), converts paths to checkout-relative slash form, and stamps
// terma.checkout.id via hookrun.CheckoutID when the checkout exists on disk.
func normalizeFilesTouched(lr *logspb.LogRecord, checkout relay.CheckoutInfo) []*logspb.LogRecord {
	if checkout.Origin == "" {
		return nil
	}
	var pathsKV *commonpb.KeyValue
	for _, kv := range lr.GetAttributes() {
		if kv.GetKey() == semconv.TermaFilesPathsKey {
			pathsKV = kv
			break
		}
	}
	if pathsKV == nil {
		return nil
	}
	type touchGroup struct {
		root   string
		remote string
		withID bool
		rels   []string
	}
	var groups []*touchGroup
	byRoot := map[string]*touchGroup{}
	addTo := func(key, root, remote string, withID bool, rel string) {
		g, ok := byRoot[key]
		if !ok {
			g = &touchGroup{root: root, remote: remote, withID: withID}
			byRoot[key] = g
			groups = append(groups, g)
		}
		if !slices.Contains(g.rels, rel) {
			g.rels = append(g.rels, rel)
		}
	}
	type locateHit struct {
		root   string
		origin string
		remote string
		inGit  bool
	}
	locateCache := map[string]locateHit{}
	locateDir := func(dir string) locateHit {
		if h, ok := locateCache[dir]; ok {
			return h
		}
		var h locateHit
		if root, gitDir, ok := gitx.LocateFS(resolvePath(dir)); ok && gitDir != "" {
			origin, remote := gitx.OriginFS(gitDir)
			h = locateHit{root: root, origin: origin, remote: remote, inGit: true}
		}
		locateCache[dir] = h
		return h
	}
	fallbackRoot := cmp.Or(checkout.Root, checkout.Cwd)
	fallbackReal := resolvePath(fallbackRoot)
	for _, elem := range pathsKV.GetValue().GetArrayValue().GetValues() {
		raw := strings.TrimSpace(elem.GetStringValue())
		if raw == "" {
			continue
		}
		abs := raw
		if !filepath.IsAbs(abs) && checkout.Cwd != "" {
			abs = filepath.Join(checkout.Cwd, abs)
		}
		abs = filepath.Clean(abs)
		if dir := deepestExistingDir(filepath.Dir(abs)); dir != "" {
			if hit := locateDir(dir); hit.inGit {
				if !strings.EqualFold(hit.origin, checkout.Origin) {
					continue
				}
				if rel, ok := relInsideRoot(abs, "", hit.root, resolvePath(hit.root)); ok {
					addTo(hit.root, hit.root, cmp.Or(hit.remote, checkout.Remote), true, rel)
				}
				continue
			}
			// Existing directory outside git: never attribute to the session's repository.
			if _, err := os.Stat(filepath.Dir(abs)); err == nil {
				continue
			}
		}
		if fallbackRoot != "" {
			if rel, ok := relInsideRoot(abs, "", fallbackRoot, fallbackReal); ok {
				addTo("fallback:"+fallbackRoot, checkout.Root, checkout.Remote, checkout.Root != "", rel)
			}
		}
	}
	if len(groups) == 0 {
		return nil
	}
	out := make([]*logspb.LogRecord, 0, len(groups))
	for i, g := range groups {
		rec := lr
		if i > 0 {
			rec = proto.CloneOf(lr)
		}
		vals := make([]*commonpb.AnyValue, len(g.rels))
		for j, r := range g.rels {
			vals[j] = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: r}}
		}
		for _, kv := range rec.Attributes {
			if kv.GetKey() == semconv.TermaFilesPathsKey {
				kv.Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: vals}}}
				break
			}
		}
		if g.remote != "" {
			setRecordAttr(rec, semconv.VCSRepositoryURLFullKey, g.remote)
		}
		if g.withID && g.root != "" {
			setRecordAttr(rec, semconv.TermaCheckoutIDKey, hookrun.CheckoutID(g.root))
		}
		out = append(out, rec)
	}
	return out
}

func relInsideRoot(raw, cwd, root, realRoot string) (string, bool) {
	p := raw
	if !filepath.IsAbs(p) && cwd != "" {
		p = filepath.Join(cwd, p)
	}
	p = filepath.Clean(p)
	tryRel := func(base, target string) (string, bool) {
		rel, err := filepath.Rel(base, target)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", false
		}
		return filepath.ToSlash(rel), true
	}
	if rel, ok := tryRel(filepath.Clean(root), p); ok {
		return rel, true
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		if rel, ok := tryRel(realRoot, resolved); ok {
			return rel, true
		}
	} else if dirResolved, derr := filepath.EvalSymlinks(filepath.Dir(p)); derr == nil {
		if rel, ok := tryRel(realRoot, filepath.Join(dirResolved, filepath.Base(p))); ok {
			return rel, true
		}
	}
	return "", false
}

func setRecordAttr(lr *logspb.LogRecord, key, val string) {
	v := &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: val}}
	for _, kv := range lr.Attributes {
		if kv.GetKey() == key {
			kv.Value = v
			return
		}
	}
	lr.Attributes = append(lr.Attributes, &commonpb.KeyValue{Key: key, Value: v})
}

func resolvePath(p string) string {
	if p == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil && r != "" {
		return r
	}
	return filepath.Clean(p)
}

func deepestExistingDir(dir string) string {
	cur := filepath.Clean(dir)
	for cur != "" {
		if fi, err := os.Stat(cur); err == nil && fi.IsDir() {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
	return ""
}
