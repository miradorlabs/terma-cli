package sourcelink

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"maps"
	"slices"
	"testing"
)

// tarball is a tree as GitHub serves one: every path under one folder.
func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: "repo-v0/" + name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeLibrary is a library that reads the tarballs it is given, by repo and tag, from no
// network, and counts how often it read each.
func fakeLibrary(t *testing.T, dir string, builds map[string][]byte) (*Library, map[string]int) {
	t.Helper()
	opened := map[string]int{}
	l := New(dir)
	l.Open = func(repo, tag string) (io.ReadCloser, error) {
		opened[repo+"@"+tag]++
		b, ok := builds[repo+"@"+tag]
		if !ok {
			return nil, errors.New("404 Not Found")
		}
		return io.NopCloser(bytes.NewReader(b)), nil
	}
	t.Cleanup(func() { l.Close() })
	return l, opened
}

// withSource registers a harness for the test's duration.
func withSource(t *testing.T, harness string, s Source) {
	t.Helper()
	prev, had := Sources[harness]
	Sources[harness] = s
	t.Cleanup(func() {
		if had {
			Sources[harness] = prev
		} else {
			delete(Sources, harness)
		}
	})
}

// lexAll lexes lines of lang, each line's code and its text.
func lexAll(lang string, src ...string) (code []string, text [][]string) {
	lx := newLexer(lang, true)
	for _, line := range src {
		c, t := lx.line(line)
		code = append(code, c)
		text = append(text, t)
	}
	return code, text
}
