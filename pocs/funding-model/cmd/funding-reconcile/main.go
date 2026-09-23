// funding-reconcile compares local exports with captured, settled calls.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/miradorlabs/terma-cli/pocs/funding-model/replay"
)

func decode(path string, dst any) error {
	b, err := replay.ReadFile(path)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return fmt.Errorf("invalid input JSON: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("input must contain exactly one JSON value")
	}
	return nil
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("funding-reconcile", flag.ContinueOnError)
	fs.SetOutput(stderr)
	scopePath := fs.String("scope", "", "report scope JSON")
	reportPath := fs.String("report", "", "provider CSV")
	capturePath := fs.String("capture", "", "settled calls and funding evidence JSON")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *scopePath == "" || *reportPath == "" || *capturePath == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "required: -scope scope.json -report provider.csv -capture capture.json")
		return 2
	}
	fail := func(err error) int { fmt.Fprintln(stderr, err); return 1 }
	var scope replay.Scope
	if err := decode(*scopePath, &scope); err != nil {
		return fail(err)
	}
	var capture replay.Capture
	if err := decode(*capturePath, &capture); err != nil {
		return fail(err)
	}
	csv, err := replay.ReadFile(*reportPath)
	if err != nil {
		return fail(err)
	}
	result, err := replay.Run(scope, csv, capture)
	if err != nil {
		return fail(err)
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		return fail(err)
	}
	return 0
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
