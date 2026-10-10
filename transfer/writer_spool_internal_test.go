package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
)

// writeSized writes vertices until the body passes minBytes, and returns the
// writer's internals for inspection.
func writeSized(t *testing.T, fc CommitClient, minBytes int64) *bufferedWriter {
	t.Helper()
	w, ok := NewDataWriter(fc, "kit").(*bufferedWriter)
	if !ok {
		t.Fatal("NewDataWriter did not return the default writer")
	}
	pad := strings.Repeat("y", 900)
	for i := 0; w.size <= minBytes; i++ {
		if err := w.WriteVertex(context.Background(), Vertex{EntityType: "T", Properties: map[string]any{"i": i, "pad": pad}}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	return w
}

// At or below InlineBodyLimit nothing touches the filesystem.
func TestWriter_NoSpoolAtOrBelowLimit(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	w, _ := NewDataWriter(&FakeCommitClient{}, "kit").(*bufferedWriter)
	for i := 0; ; i++ {
		before := w.size
		_ = w.WriteVertex(context.Background(), Vertex{EntityType: "T", Properties: map[string]any{"i": i}})
		if w.size > InlineBodyLimit {
			if w.spool == nil {
				t.Fatal("the body passed InlineBodyLimit without spooling")
			}
			break
		}
		if w.spool != nil {
			t.Fatalf("spooled at %d bytes (before the write: %d), within InlineBodyLimit", w.size, before)
		}
	}
}

// The spool descriptor is closed on every way the writer can end once it has
// spooled — the guarantee that a temp file the size of the artifact is never
// held after the writer is done with it.
func TestWriter_SpoolDescriptorReleasedOnEveryEnd(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(*FakeCommitClient)
		end   func(*bufferedWriter)
	}{
		{name: "committed", end: func(w *bufferedWriter) { _, _ = w.Close(ctx) }},
		{name: "prepare fails", setup: func(f *FakeCommitClient) { f.FailPrepare = errors.New("x") }, end: func(w *bufferedWriter) { _, _ = w.Close(ctx) }},
		{name: "upload fails", setup: func(f *FakeCommitClient) { f.FailPut = errors.New("x") }, end: func(w *bufferedWriter) { _, _ = w.Close(ctx) }},
		{name: "complete fails", setup: func(f *FakeCommitClient) { f.FailComplete = errors.New("x") }, end: func(w *bufferedWriter) { _, _ = w.Close(ctx) }},
		{name: "discarded", end: func(w *bufferedWriter) { _ = w.Discard() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TMPDIR", t.TempDir())
			fc := &FakeCommitClient{}
			if tc.setup != nil {
				tc.setup(fc)
			}
			w := writeSized(t, fc, InlineBodyLimit)
			f := w.spool
			if f == nil {
				t.Fatal("expected the body to have spooled")
			}
			tc.end(w)
			if w.spool != nil {
				t.Error("the writer still references its spool")
			}
			if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Errorf("spool descriptor still open after %s (Stat: %v)", tc.name, err)
			}
		})
	}
}

// Close takes the content hash from the running digest and never re-reads the
// body. Proved by changing a spooled byte behind the writer's back: a second
// pass would hash the changed bytes, the running digest still reports the
// bytes that were written.
func TestWriter_HashIsNotASecondPass(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	fc := &FakeCommitClient{}
	w := writeSized(t, fc, 2*InlineBodyLimit)
	if err := w.spoolW.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	written := make([]byte, w.size)
	if _, err := w.spool.ReadAt(written, 0); err != nil {
		t.Fatalf("read spool: %v", err)
	}
	sum := sha256.Sum256(written)
	if _, err := w.spool.WriteAt([]byte("Z"), w.size/2); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	r, err := w.Close(context.Background())
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if r.ContentHash != hex.EncodeToString(sum[:]) {
		t.Errorf("content hash %s is not the digest of the written bytes %x", r.ContentHash, sum)
	}
	if fc.LastBodyHash() == r.ContentHash {
		t.Error("the uploaded (tampered) body matches the digest: the tamper did not take, so the test proves nothing")
	}
}

// A spool write failure is sticky: the failing write reports it, every later
// write reports it, Close refuses to commit, and the platform sees nothing.
func TestWriter_SpoolWriteFailureIsSticky(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	fc := &FakeCommitClient{}
	w := writeSized(t, fc, InlineBodyLimit)
	_ = w.spool.Close() // the next buffer flush hits a closed descriptor

	var firstErr error
	for i := 0; i < 1000 && firstErr == nil; i++ { // > spoolBufferSize of records forces a flush
		firstErr = w.WriteVertex(context.Background(), Vertex{EntityType: "T", Properties: map[string]any{"pad": strings.Repeat("z", 900)}})
	}
	if firstErr == nil || !strings.Contains(firstErr.Error(), "write artifact spool") {
		t.Fatalf("writes past a closed spool: %v, want a spool write error", firstErr)
	}
	if err := w.WriteVertex(context.Background(), Vertex{EntityType: "T"}); !errors.Is(err, os.ErrClosed) {
		t.Errorf("a later write: %v, want the same sticky error", err)
	}
	if _, err := w.Close(context.Background()); err == nil || !strings.Contains(err.Error(), "not committed") {
		t.Errorf("Close: %v, want a not-committed error", err)
	}
	if fc.PrepareCalls()+fc.PutCalls()+fc.CompleteCalls() != 0 {
		t.Error("a writer with a failed spool committed something")
	}
}

// Where the early unlink failed (Windows), the kept path is removed when the
// body is released.
func TestWriter_ReleaseRemovesAKeptSpoolPath(t *testing.T) {
	dir := t.TempDir()
	f, err := os.CreateTemp(dir, spoolFilePrefix+"*")
	if err != nil {
		t.Fatal(err)
	}
	w := &bufferedWriter{spool: f, spoolPath: f.Name()}
	if err := w.releaseBody(); err != nil {
		t.Fatalf("releaseBody: %v", err)
	}
	if _, err := os.Stat(f.Name()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("kept spool path survived release: %v", err)
	}
}
