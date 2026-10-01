package transfer_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

func loadGolden(t *testing.T) (goldenFile, []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "golden-v0.98.0-beta.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var g goldenFile
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("decode golden: %v", err)
	}
	inline, err := os.ReadFile(filepath.Join("testdata", "golden-v0.98.0-beta-inline.ndjson"))
	if err != nil {
		t.Fatalf("read inline golden: %v", err)
	}
	return g, inline
}

// The spooling writer must produce exactly what the in-memory writer it
// replaced produced. The goldens were recorded by running writeGoldenRecords
// through the RELEASED v0.98.0-beta writer, so they are an oracle this code
// had no part in: the inline body is compared byte for byte, and the presigned
// body — too large to check in — through its SHA-256, size and entry count.
func TestWriter_MatchesV098Goldens(t *testing.T) {
	g, inlineBody := loadGolden(t)
	if g.SDKVersion != "v0.98.0-beta" || len(g.Cases) != 2 {
		t.Fatalf("unexpected golden file: %+v", g)
	}
	for _, c := range g.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Setenv("TMPDIR", t.TempDir())
			fc := &transfer.FakeCommitClient{}
			w := transfer.NewWriter(fc, transfer.LoadKind(g.Kind), g.KitID)
			if err := writeGoldenRecords(context.Background(), w, c.Records); err != nil {
				t.Fatalf("write: %v", err)
			}
			r, err := w.Close(context.Background())
			if err != nil {
				t.Fatalf("Close: %v", err)
			}
			if string(r.Mode) != c.Mode || r.BytesWritten != c.Bytes || r.EntryCount != c.EntryCount {
				t.Fatalf("receipt mode=%s bytes=%d entries=%d, golden mode=%s bytes=%d entries=%d",
					r.Mode, r.BytesWritten, r.EntryCount, c.Mode, c.Bytes, c.EntryCount)
			}
			if r.ContentHash != c.ContentHash {
				t.Errorf("content hash %s, golden %s", r.ContentHash, c.ContentHash)
			}
			// What the platform received must hash to what the writer claimed.
			if got := fc.LastBodyHash(); got != c.ContentHash {
				t.Errorf("committed body hashes to %s, golden %s", got, c.ContentHash)
			}
			if c.Mode == string(transfer.TransportInline) && !bytes.Equal(fc.LastBody(), inlineBody) {
				t.Errorf("inline body differs from v0.98.0-beta's (%d vs %d bytes)", len(fc.LastBody()), len(inlineBody))
			}
		})
	}
}

// emptyDir fails the test if dir has any entry. The spool is unlinked the
// moment it is created, so not even a large artifact may leave a name behind.
func emptyDir(t *testing.T, dir, when string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("%s: TMPDIR holds %v, want nothing", when, names)
	}
}

// A spooled artifact never shows up in TMPDIR: not while it is being written,
// not after it is committed, not after any commit failure, and not after the
// writer is discarded. Not parallel: it owns TMPDIR.
func TestWriter_SpoolLeavesNoDirectoryEntry(t *testing.T) {
	g, _ := loadGolden(t)
	large := g.Cases[1].Records
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		setup func(*transfer.FakeCommitClient)
		end   func(transfer.Writer) error
	}{
		{name: "committed", end: func(w transfer.Writer) error { _, err := w.Close(ctx); return err }},
		{name: "prepare fails", setup: func(fc *transfer.FakeCommitClient) { fc.FailPrepare = errBoom },
			end: func(w transfer.Writer) error { _, err := w.Close(ctx); return err }},
		{name: "upload fails", setup: func(fc *transfer.FakeCommitClient) { fc.FailPut = errBoom },
			end: func(w transfer.Writer) error { _, err := w.Close(ctx); return err }},
		{name: "complete fails", setup: func(fc *transfer.FakeCommitClient) { fc.FailComplete = errBoom },
			end: func(w transfer.Writer) error { _, err := w.Close(ctx); return err }},
		{name: "discarded", end: func(w transfer.Writer) error { return w.(transfer.Discarder).Discard() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("TMPDIR", dir)
			fc := &transfer.FakeCommitClient{}
			if tc.setup != nil {
				tc.setup(fc)
			}
			w := transfer.NewDataWriter(fc, "kit")
			if err := writeGoldenRecords(ctx, w, large); err != nil {
				t.Fatalf("write: %v", err)
			}
			emptyDir(t, dir, "while spooling")
			_ = tc.end(w)
			emptyDir(t, dir, "after "+tc.name)
		})
	}
}

var errBoom = errorString("boom")

type errorString string

func (e errorString) Error() string { return string(e) }

// The inline/presigned switch sits exactly at InlineBodyLimit: a body of
// exactly the limit commits inline, one byte more goes presigned.
func TestWriter_InlineLimitBoundary(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	ctx := context.Background()
	write := func(pad int) *transfer.Receipt {
		t.Helper()
		fc := &transfer.FakeCommitClient{}
		w := transfer.NewDataWriter(fc, "kit")
		if err := w.WriteVertex(ctx, transfer.Vertex{EntityType: "T", Properties: map[string]any{"pad": strings.Repeat("x", pad)}}); err != nil {
			t.Fatalf("write: %v", err)
		}
		r, err := w.Close(ctx)
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
		if r.ContentHash != fc.LastBodyHash() {
			t.Fatalf("hash %s does not match the committed body %s", r.ContentHash, fc.LastBodyHash())
		}
		return r
	}
	overhead := write(0).BytesWritten // each 'x' adds exactly one byte
	atLimit := int(transfer.InlineBodyLimit - overhead)

	if r := write(atLimit); r.BytesWritten != transfer.InlineBodyLimit || r.Mode != transfer.TransportInline {
		t.Errorf("a body of exactly InlineBodyLimit: %d bytes, mode %s; want %d bytes inline",
			r.BytesWritten, r.Mode, transfer.InlineBodyLimit)
	}
	if r := write(atLimit + 1); r.BytesWritten != transfer.InlineBodyLimit+1 || r.Mode != transfer.TransportPresigned {
		t.Errorf("a body one byte over InlineBodyLimit: %d bytes, mode %s; want %d bytes presigned",
			r.BytesWritten, r.Mode, transfer.InlineBodyLimit+1)
	}
}

// Discard abandons the artifact: nothing is committed, the writer refuses any
// further use, and repeating it — or calling it after Close — is harmless,
// which is what lets a server defer it unconditionally.
func TestWriter_Discard(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	ctx := context.Background()
	g, _ := loadGolden(t)

	for _, records := range []int{g.Cases[0].Records, g.Cases[1].Records} { // inline-sized and spooled
		fc := &transfer.FakeCommitClient{}
		w := transfer.NewDataWriter(fc, "kit")
		d, ok := w.(transfer.Discarder)
		if !ok {
			t.Fatal("the default writer must implement transfer.Discarder")
		}
		if err := writeGoldenRecords(ctx, w, records); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := d.Discard(); err != nil {
			t.Fatalf("Discard: %v", err)
		}
		if err := d.Discard(); err != nil {
			t.Errorf("a second Discard: %v, want nil", err)
		}
		if err := w.WriteVertex(ctx, transfer.Vertex{EntityType: "T"}); err == nil || !strings.Contains(err.Error(), "after Discard") {
			t.Errorf("write after Discard: %v", err)
		}
		if _, err := w.Close(ctx); err == nil || !strings.Contains(err.Error(), "discarded") {
			t.Errorf("Close after Discard: %v", err)
		}
		if fc.PrepareCalls()+fc.PutCalls()+fc.CompleteCalls() != 0 {
			t.Errorf("a discarded writer committed something: prepare=%d put=%d complete=%d",
				fc.PrepareCalls(), fc.PutCalls(), fc.CompleteCalls())
		}
	}

	// After a successful Close, Discard changes nothing.
	fc := &transfer.FakeCommitClient{}
	w := transfer.NewDataWriter(fc, "kit")
	_ = w.WriteVertex(ctx, transfer.Vertex{EntityType: "T"})
	if _, err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := transfer.DiscardWriter(w); err != nil {
		t.Errorf("Discard after Close: %v, want nil", err)
	}
	if fc.CompleteCalls() != 1 {
		t.Errorf("Complete calls = %d, want 1", fc.CompleteCalls())
	}
}

// The spooled path through the REAL HTTPCommitClient, not FakeCommitClient:
// prepare_upload, a presigned PUT of the section reader over the spool, then
// complete. FakeCommitClient reads its body in one call, so it cannot show how
// net/http treats the reader; this test does.
func TestWriter_SpooledArtifactThroughHTTPCommitClient(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	g, _ := loadGolden(t)
	gate, gatewaySrv, _ := newFakeMCPGateway(t)
	cc, err := transfer.NewHTTPCommitClient(gatewaySrv.URL, "tenant-A", "golden-kit")
	if err != nil {
		t.Fatal(err)
	}
	w := transfer.NewDataWriter(cc, "golden-kit")
	if err := writeGoldenRecords(context.Background(), w, g.Cases[1].Records); err != nil {
		t.Fatalf("write: %v", err)
	}
	r, err := w.Close(context.Background())
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if r.Mode != transfer.TransportPresigned || gate.uploadCalls != 1 || gate.completeCalls != 1 {
		t.Fatalf("mode=%s uploads=%d completes=%d, want one presigned upload and one complete",
			r.Mode, gate.uploadCalls, gate.completeCalls)
	}
	sum := sha256.Sum256(gate.uploadBody)
	if got := hex.EncodeToString(sum[:]); got != g.Cases[1].ContentHash || int64(len(gate.uploadBody)) != g.Cases[1].Bytes {
		t.Errorf("storage received %d bytes hashing to %s; golden is %d bytes, %s",
			len(gate.uploadBody), got, g.Cases[1].Bytes, g.Cases[1].ContentHash)
	}
	if r.ContentHash != g.Cases[1].ContentHash {
		t.Errorf("receipt hash %s, golden %s", r.ContentHash, g.Cases[1].ContentHash)
	}
}

// DiscardWriter is a no-op for a writer that does not implement Discarder.
func TestDiscardWriter_IgnoresWritersWithoutDiscard(t *testing.T) {
	t.Parallel()
	if err := transfer.DiscardWriter(transfer.NewNopWriter("")); err != nil {
		t.Errorf("DiscardWriter(NopWriter) = %v, want nil", err)
	}
}

// A spool that cannot be created fails the write that needed it — naming the
// directory and what to set — and every later write and Close, and commits
// nothing. A body that would have fitted inline is never at risk.
func TestWriter_SpoolCreationFailureFailsLoudly(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	t.Setenv("TMPDIR", missing)
	ctx := context.Background()
	g, _ := loadGolden(t)

	fc := &transfer.FakeCommitClient{}
	w := transfer.NewDataWriter(fc, "kit")
	err := writeGoldenRecords(ctx, w, g.Cases[1].Records)
	if err == nil {
		t.Fatal("a body past InlineBodyLimit with no writable TMPDIR must fail")
	}
	for _, want := range []string{missing, "TMPDIR"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if werr := w.WriteVertex(ctx, transfer.Vertex{EntityType: "T"}); werr == nil {
		t.Error("a later write succeeded after the spool failed")
	}
	if _, cerr := w.Close(ctx); cerr == nil || !strings.Contains(cerr.Error(), "not committed") {
		t.Errorf("Close after a spool failure: %v, want a not-committed error", cerr)
	}
	if fc.PrepareCalls()+fc.PutCalls()+fc.CompleteCalls() != 0 {
		t.Error("a writer whose spool failed committed something")
	}

	// The same TMPDIR is irrelevant to an inline-sized body.
	fc = &transfer.FakeCommitClient{}
	w = transfer.NewDataWriter(fc, "kit")
	if err := writeGoldenRecords(ctx, w, g.Cases[0].Records); err != nil {
		t.Fatalf("inline-sized write: %v", err)
	}
	if _, err := w.Close(ctx); err != nil {
		t.Fatalf("inline-sized Close: %v", err)
	}
}
