// Command versionsort sorts the versions on its standard input, one a line, as the Harness
// Reference ranks them (refdiff.VersionCompare), oldest first: the shell scripts that ask the
// registries what is new (reference/new-builds.sh, backfill.sh) sort with it, so there is one
// ordering of versions. -u prints each version once; blank lines are dropped.
//
//	printf '0.2.1\n0.2.1-rc.10\n' | go run ./refdiff/cmd/versionsort
//
// It imports refdiff alone, which imports the standard library alone: a checkout of
// test/e2e/go.mod, go.sum and refdiff/ builds it, with no module to download.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/e2e/refdiff"
)

func main() {
	unique := flag.Bool("u", false, "print each version once")
	flag.Parse()
	if err := sortVersions(os.Stdin, os.Stdout, *unique); err != nil {
		fmt.Fprintln(os.Stderr, "versionsort:", err)
		os.Exit(1)
	}
}

// sortVersions writes the versions r holds, one a line, to w, oldest first.
func sortVersions(r io.Reader, w io.Writer, unique bool) error {
	var vs []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if v := strings.TrimSpace(sc.Text()); v != "" {
			vs = append(vs, v)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	slices.SortStableFunc(vs, refdiff.VersionCompare)
	if unique {
		vs = slices.Compact(vs)
	}
	bw := bufio.NewWriter(w)
	for _, v := range vs {
		fmt.Fprintln(bw, v)
	}
	return bw.Flush()
}
