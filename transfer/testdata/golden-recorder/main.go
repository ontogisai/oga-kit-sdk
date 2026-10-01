// Command golden-recorder runs writeGoldenRecords through the RELEASED
// oga-kit-sdk v0.98.0-beta writer and writes what that writer committed to the
// directory named by its argument. It is the oracle behind
// TestWriter_MatchesV098Goldens: the goldens come from code this module's
// writer had no part in.
//
// It is its own module, pinned to v0.98.0-beta, because a module cannot depend
// on an older version of itself; living under testdata keeps it out of
// `go build ./...` and `go test ./...`. Run it through record.sh, which copies
// the generator in from ../../golden_records_test.go so the two cannot drift.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: golden-recorder <output dir>")
		os.Exit(2)
	}
	dir := os.Args[1]
	ctx := context.Background()
	out := goldenFile{SDKVersion: "v0.98.0-beta", Kind: "data", KitID: "golden-kit"}
	for _, c := range []struct {
		name string
		n    int
	}{{"inline", 12}, {"presigned", 4000}} {
		fc := &transfer.FakeCommitClient{}
		w := transfer.NewDataWriter(fc, out.KitID)
		if err := writeGoldenRecords(ctx, w, c.n); err != nil {
			fail(err)
		}
		r, err := w.Close(ctx)
		if err != nil {
			fail(err)
		}
		if fc.LastBodyHash() != r.ContentHash {
			fail(fmt.Errorf("%s: v0.98.0-beta's receipt hash does not match the body it committed", c.name))
		}
		out.Cases = append(out.Cases, goldenCase{Name: c.name, Records: c.n, Mode: string(r.Mode),
			Bytes: r.BytesWritten, EntryCount: r.EntryCount, ContentHash: r.ContentHash})
		if c.name == "inline" {
			if err := os.WriteFile(filepath.Join(dir, "golden-v0.98.0-beta-inline.ndjson"), fc.LastBody(), 0o644); err != nil {
				fail(err)
			}
		}
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "golden-v0.98.0-beta.json"), append(b, '\n'), 0o644); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "golden-recorder:", err)
	os.Exit(1)
}
