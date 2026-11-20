package antigravity

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/tailscale/hujson"
)

// agy's settings file is edited as a syntax tree, member by member: packed again, whatever
// terma did not change is byte for byte what was read.

// antigravitySettings is agy's settings file as its syntax tree.
type antigravitySettings struct {
	path, writePath string
	data            []byte
	existed         bool
	mode            fs.FileMode
	root            hujson.Value
	obj             *hujson.Object
}

func loadAntigravitySettings(path string) (*antigravitySettings, error) {
	writePath, _, err := config.ResolveWritePath(path)
	if err != nil {
		return nil, err
	}
	s := &antigravitySettings{path: path, writePath: writePath, mode: harness.SettingsMode}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", path, err)
	default:
		s.data, s.existed = data, true
		if info, err := os.Stat(path); err == nil {
			s.mode = info.Mode().Perm()
		}
	}
	text := data
	if len(bytes.TrimSpace(text)) == 0 {
		text = []byte("{}\n")
	}
	if s.root, err = hujson.Parse(text); err != nil {
		return nil, fmt.Errorf("parse %s: %w (fix or move the file, then retry)", path, err)
	}
	obj, ok := s.root.Value.(*hujson.Object)
	if !ok {
		return nil, fmt.Errorf("parse %s: expected a JSON object", path)
	}
	s.obj = obj
	return s, nil
}

// find is the member agy reads for key: the last of that name.
func (s *antigravitySettings) find(key string) int {
	for i := len(s.obj.Members) - 1; i >= 0; i-- {
		if lit, ok := s.obj.Members[i].Name.Value.(hujson.Literal); ok && lit.String() == key {
			return i
		}
	}
	return -1
}

// raw is key's value as written, "" when absent.
func (s *antigravitySettings) raw(key string) string {
	i := s.find(key)
	if i < 0 {
		return ""
	}
	return string(hujson.Value{Value: s.obj.Members[i].Value.Value}.Pack())
}

// set gives key the raw value in place, or adds it last, indented as the members before it.
func (s *antigravitySettings) set(key, raw string) error {
	value, err := hujson.Parse([]byte(raw))
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	if i := s.find(key); i >= 0 {
		s.obj.Members[i].Value.Value = value.Value
		return nil
	}
	indent := "  "
	if n := len(s.obj.Members); n > 0 {
		before := string(s.obj.Members[n-1].Name.BeforeExtra)
		if i := strings.LastIndexByte(before, '\n'); i >= 0 && strings.Trim(before[i+1:], " \t") == "" {
			indent = before[i+1:]
		}
	}
	if !bytes.Contains(s.obj.AfterExtra, []byte("\n")) {
		s.obj.AfterExtra = hujson.Extra("\n")
	}
	s.obj.Members = append(s.obj.Members, hujson.ObjectMember{
		Name:  hujson.Value{BeforeExtra: hujson.Extra("\n" + indent), Value: hujson.String(key)},
		Value: hujson.Value{BeforeExtra: hujson.Extra(" "), Value: value.Value},
	})
	return nil
}

// remove takes key's member out.
func (s *antigravitySettings) remove(key string) {
	i := s.find(key)
	if i < 0 {
		return
	}
	s.obj.Members = slices.Delete(s.obj.Members, i, i+1)
	if len(s.obj.Members) == 0 {
		s.obj.AfterExtra = nil
	}
}

func (s *antigravitySettings) pack() []byte {
	out := s.root.Pack()
	if !bytes.HasSuffix(out, []byte("\n")) {
		out = append(out, '\n')
	}
	return out
}

func (s *antigravitySettings) write(data []byte) error {
	if err := os.MkdirAll(filepath.Dir(s.writePath), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(s.writePath), err)
	}
	return config.WriteFileAtomic(s.writePath, data, s.mode)
}
