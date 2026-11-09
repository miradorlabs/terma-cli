//go:build js && wasm

// Command wasm is refdiff compiled to WebAssembly, so that a reader's browser compares any
// two versions of a harness with the comparison that made the published diffs. Run with Go's
// wasm_exec.js, it defines one global:
//
//	harnessReferenceCompare(fromSnapshotJSON, toSnapshotJSON, changelogJSON) → diffJSON
//
// changelogJSON is a harness's changelog.json; the diff is in the diffs' schema, or
// {"error": "…"} for input it cannot read.
package main

import (
	"encoding/json"
	"syscall/js"
	"time"

	"github.com/miradorlabs/terma-cli/e2e/refdiff"
)

func main() {
	js.Global().Set("harnessReferenceCompare", js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) != 3 {
			return failure("want the two snapshots and the changelog")
		}
		var from, to refdiff.Snapshot
		if err := json.Unmarshal([]byte(args[0].String()), &from); err != nil {
			return failure("from snapshot: " + err.Error())
		}
		if err := json.Unmarshal([]byte(args[1].String()), &to); err != nil {
			return failure("to snapshot: " + err.Error())
		}
		var changelog struct {
			Versions map[string]refdiff.ChangelogNote `json:"versions"`
		}
		if s := args[2].String(); s != "" && s != "null" && s != "undefined" {
			if err := json.Unmarshal([]byte(s), &changelog); err != nil {
				return failure("changelog: " + err.Error())
			}
		}
		data, err := json.Marshal(refdiff.CompareRange(&from, &to, changelog.Versions, time.Now().UTC()))
		if err != nil {
			return failure(err.Error())
		}
		return string(data)
	}))
	select {}
}

func failure(msg string) string {
	data, _ := json.Marshal(map[string]string{"error": msg})
	return string(data)
}
