package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// largeArtifactEnv gates the measurement below. It is opt-in rather than part of
// `go test ./...` because it moves 300 MB through the loopback interface and onto
// disk: worth paying deliberately when the streaming claim is being checked,
// not on every CI run of an unrelated change.
const largeArtifactEnv = "OGA_FETCH_LARGE_ARTIFACT_TEST"

// largeArtifactSize is the SJ24K-53 Phase 1 cap, and the size Studio's largest
// measured export (210 MB) has to fit inside.
const largeArtifactSize int64 = 300 << 20

// TestGet_LargeArtifactDoesNotScaleMemory is the measurement behind the SJ24K-53
// acceptance criterion "peak RSS a small multiple of the copy buffer, not of the
// artifact. Measured, not assumed."
//
// Run it with:
//
//	OGA_FETCH_LARGE_ARTIFACT_TEST=1 go test -run LargeArtifact -v ./fetch/
//
// Two quantities are recorded, and the second is the one that actually settles
// the question:
//
//   - Peak HeapAlloc, sampled. Indicative but noisy — the sampler can miss a
//     short-lived spike, and the Go heap is not RSS.
//   - TOTAL bytes allocated over the transfer, which is exact and cumulative, so
//     nothing can hide between samples. A buffering implementation must allocate
//     at least the artifact's length here (io.ReadAll's doubling growth allocates
//     appreciably more), while a streaming one allocates its buffer once and then
//     essentially nothing per chunk. The two differ by orders of magnitude rather
//     than by a margin, which is what makes a threshold safe to assert.
func TestGet_LargeArtifactDoesNotScaleMemory(t *testing.T) {
	if os.Getenv(largeArtifactEnv) == "" {
		t.Skipf("set %s=1 to run the 300 MB streaming measurement", largeArtifactEnv)
	}

	// A repeating pattern rather than zeros: a sparse-file optimisation somewhere
	// in the stack could make an all-zero artifact unrepresentative on disk.
	chunk := make([]byte, 1<<20)
	for i := range chunk {
		chunk[i] = byte(i % 251)
	}
	chunks := int(largeArtifactSize / int64(len(chunk)))

	// The expected digest, computed the honest way: over the same bytes the server
	// will send, with no involvement from the code under test.
	want := sha256.New()
	for range chunks {
		want.Write(chunk)
	}
	wantHex := hex.EncodeToString(want.Sum(nil))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(largeArtifactSize))
		w.WriteHeader(http.StatusOK)
		for range chunks {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	d := New(
		WithAllowInsecure(true),
		WithMaxBytes(largeArtifactSize),
		WithTempDir(dir),
		WithRetry(RetryConfig{MaxAttempts: 1}),
		// The default 2 minute per-attempt timeout is ample for loopback, but a
		// slow CI disk should fail on the assertion rather than on a deadline.
		WithHTTPClient(&http.Client{Timeout: 10 * time.Minute}),
	)

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	var peakHeap atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				return
			default:
			}
			// ReadMemStats stops the world, so a 1 ms loop paused the very
			// window it measures roughly once per millisecond. 25 ms still
			// samples a multi-hundred-millisecond transfer several times over,
			// and the assertion below is on cumulative allocation anyway —
			// which is exact and needs no sampling at all.
			runtime.ReadMemStats(&ms)
			for {
				cur := peakHeap.Load()
				if ms.HeapAlloc <= cur || peakHeap.CompareAndSwap(cur, ms.HeapAlloc) {
					break
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
	}()

	start := time.Now()
	res, err := d.Get(context.Background(), srv.URL)
	elapsed := time.Since(start)
	close(stop)
	<-done
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = res.Close() }()

	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc

	t.Logf("artifact           = %d bytes (%d MiB)", res.Size, res.Size>>20)
	t.Logf("elapsed            = %s", elapsed)
	t.Logf("copy buffer        = %d bytes (%d KiB)", copyBufferSize, copyBufferSize>>10)
	t.Logf("total allocated    = %d bytes (%d MiB)", allocated, allocated>>20)
	t.Logf("peak heap (sampled)= %d bytes (%d MiB)", peakHeap.Load(), peakHeap.Load()>>20)

	if res.Size != largeArtifactSize {
		t.Errorf("Size = %d, want %d", res.Size, largeArtifactSize)
	}
	if res.Hash != wantHex {
		t.Errorf("Hash = %q, want %q — the streamed digest must equal an independent sha256", res.Hash, wantHex)
	}

	// 32 MiB against a 300 MB artifact. Deliberately loose: the HTTP transport,
	// the in-process test server and the runtime all allocate here too, and
	// tightening this to the copy buffer would make the test a flake detector
	// rather than a proof. It is still ~10x below what buffering the artifact
	// costs, so the two implementations cannot both satisfy it.
	const allocCeiling = 32 << 20
	if allocated > allocCeiling {
		t.Errorf("allocated %d bytes for a %d byte artifact (ceiling %d) — "+
			"the download is scaling with the artifact, not the copy buffer",
			allocated, largeArtifactSize, allocCeiling)
	}

	// And the spool must really hold the artifact: a streaming implementation that
	// dropped bytes would pass every memory assertion above.
	spools := spoolFiles(t, dir)
	if len(spools) != 1 {
		t.Fatalf("spool files = %d, want exactly 1", len(spools))
	}
	fi, err := os.Stat(spools[0])
	if err != nil {
		t.Fatalf("stat spool: %v", err)
	}
	if fi.Size() != largeArtifactSize {
		t.Errorf("spool file is %d bytes, want %d", fi.Size(), largeArtifactSize)
	}

	// Re-reading the spool must reproduce the digest, since the multi-pass
	// consumers depend on the file rather than on Hash.
	rehash := sha256.New()
	if _, err := io.Copy(rehash, res.Reader()); err != nil {
		t.Fatalf("re-read spool: %v", err)
	}
	if got := hex.EncodeToString(rehash.Sum(nil)); got != wantHex {
		t.Errorf("re-reading the spool gave %q, want %q", got, wantHex)
	}
}
