package relay

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func (o outbox) routes() ([]route, error) {
	top, err := os.ReadDir(o.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := o.routesUnder(top, "")
	for _, a := range top {
		if account, ok := strings.CutPrefix(a.Name(), accountPrefix); ok && a.IsDir() && validAccount(account) && account != "" {
			projects, err := os.ReadDir(filepath.Join(o.dir, a.Name()))
			if err == nil {
				out = append(out, o.routesUnder(projects, account)...)
			}
		}
	}
	return out, nil
}

// routesUnder lists the routes below projects, the project directories of account's
// directory, or of the outbox itself for "".
func (o outbox) routesUnder(projects []os.DirEntry, account string) []route {
	base := o.dir
	if account != "" {
		base = filepath.Join(o.dir, accountPrefix+account)
	}
	var out []route
	for _, p := range projects {
		if !p.IsDir() || strings.HasPrefix(p.Name(), ".") || strings.HasPrefix(p.Name(), accountPrefix) {
			continue
		}
		tools, err := os.ReadDir(filepath.Join(base, p.Name()))
		if err != nil {
			continue
		}
		for _, t := range tools {
			if !t.IsDir() {
				continue
			}
			repos, err := os.ReadDir(filepath.Join(base, p.Name(), t.Name()))
			if err != nil {
				continue
			}
			for _, k := range repos {
				rt := route{account: account, project: p.Name(), tool: t.Name(), repo: k.Name()}
				if k.IsDir() && validRoute(rt) {
					out = append(out, rt)
				}
			}
		}
	}
	return out
}
