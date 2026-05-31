package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"sync"
)

// FakeCommitClient is an in-memory CommitClient suitable for tests.
// It captures every prepare_upload, presigned-PUT, and complete call
// so tests can assert on the artifact body, the transport mode, and
// the request shapes without standing up a real gateway.
//
// FakeCommitClient is goroutine-safe.
type FakeCommitClient struct {
	mu sync.Mutex

	// JobIDFn is consulted on every Complete call to mint the
	// returned job_id. Defaults to a small counter ("job-1",
	// "job-2", ...) when nil.
	JobIDFn func() string

	// FailPrepare, FailPut, FailComplete inject failures from the
	// next call. Each toggle resets after firing once.
	FailPrepare, FailPut, FailComplete error

	prepareCalls  int
	putCalls      int
	completeCalls int

	lastUploadURL   string
	lastUploadToken string
	lastBody        []byte
	lastComplete    *CompleteRequest
	jobCounter      int
}

// PrepareUpload returns a synthetic upload URL + token.
func (f *FakeCommitClient) PrepareUpload(_ context.Context) (*PrepareUploadResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepareCalls++
	if f.FailPrepare != nil {
		err := f.FailPrepare
		f.FailPrepare = nil
		return nil, err
	}
	f.lastUploadURL = "https://fake-storage.example/oga-transfer/x"
	f.lastUploadToken = "tok-" + hex.EncodeToString(randBytes(8))
	return &PrepareUploadResponse{
		UploadURL:   f.lastUploadURL,
		UploadToken: f.lastUploadToken,
	}, nil
}

// PutBytes captures the streamed body so tests can assert on it.
func (f *FakeCommitClient) PutBytes(_ context.Context, uploadURL string, body io.Reader, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putCalls++
	if f.FailPut != nil {
		err := f.FailPut
		f.FailPut = nil
		return err
	}
	if uploadURL != f.lastUploadURL {
		return errors.New("FakeCommitClient: PutBytes called with URL that did not come from PrepareUpload")
	}
	buf, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	f.lastBody = buf
	return nil
}

// Complete captures the request and returns a synthetic CompleteResponse.
func (f *FakeCommitClient) Complete(_ context.Context, req *CompleteRequest) (*CompleteResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completeCalls++
	if f.FailComplete != nil {
		err := f.FailComplete
		f.FailComplete = nil
		return nil, err
	}
	// Capture inline body when present so tests can assert on it
	// regardless of which path the writer chose.
	clone := *req
	if len(req.InlineBody) > 0 {
		clone.InlineBody = append([]byte(nil), req.InlineBody...)
		f.lastBody = clone.InlineBody
	}
	f.lastComplete = &clone
	jobID := ""
	if f.JobIDFn != nil {
		jobID = f.JobIDFn()
	}
	if jobID == "" {
		f.jobCounter++
		jobID = "job-" + hex.EncodeToString(randBytes(4))
	}
	return &CompleteResponse{
		JobID:  jobID,
		Status: "running",
	}, nil
}

// PrepareCalls reports how many times PrepareUpload was called.
func (f *FakeCommitClient) PrepareCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prepareCalls
}

// PutCalls reports how many times PutBytes was called.
func (f *FakeCommitClient) PutCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.putCalls
}

// CompleteCalls reports how many times Complete was called.
func (f *FakeCommitClient) CompleteCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.completeCalls
}

// LastBody returns the most recent artifact body the client received,
// whether via inline Complete or presigned PutBytes.
func (f *FakeCommitClient) LastBody() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.lastBody...)
}

// LastComplete returns the most recent CompleteRequest the client
// received. Useful for asserting on Kind, ContentHash, EntryCount.
func (f *FakeCommitClient) LastComplete() *CompleteRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastComplete == nil {
		return nil
	}
	clone := *f.lastComplete
	return &clone
}

// LastBodyHash returns sha256(lastBody) hex-encoded — useful when
// asserting that the writer's content_hash matches the bytes the
// client received.
func (f *FakeCommitClient) LastBodyHash() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.lastBody) == 0 {
		return ""
	}
	h := sha256.Sum256(f.lastBody)
	return hex.EncodeToString(h[:])
}

// randBytes returns n bytes of pseudo-random data sufficient for
// generating unique-enough IDs in test fixtures. Not for production
// use.
func randBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + 7) //nolint:gosec // test fixture only
	}
	return b
}

// NopWriter is a transfer.Writer that discards everything. Useful for
// loader server tests that only exercise the request-routing path,
// not the persistence path.
type NopWriter struct {
	mu      sync.Mutex
	count   int
	bytes   int64
	closed  bool
	jobID   string
	receipt *Receipt
}

// NewNopWriter returns a NopWriter that mints the supplied job_id
// when Closed (defaults to "nop-job").
func NewNopWriter(jobID string) *NopWriter {
	if jobID == "" {
		jobID = "nop-job"
	}
	return &NopWriter{jobID: jobID}
}

// WriteVertex implements transfer.Writer.
func (n *NopWriter) WriteVertex(_ context.Context, v Vertex) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return errors.New("NopWriter: write after close")
	}
	n.count++
	if v.ID != "" {
		n.bytes += int64(len(v.ID))
	}
	return nil
}

// WriteEdge implements transfer.Writer.
func (n *NopWriter) WriteEdge(_ context.Context, _ Edge) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return errors.New("NopWriter: write after close")
	}
	n.count++
	return nil
}

// WriteEntityType implements transfer.Writer.
func (n *NopWriter) WriteEntityType(_ context.Context, _ EntityTypeDef) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return errors.New("NopWriter: write after close")
	}
	n.count++
	return nil
}

// WriteHierarchy implements transfer.Writer.
func (n *NopWriter) WriteHierarchy(_ context.Context, _ HierarchyEntry) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return errors.New("NopWriter: write after close")
	}
	n.count++
	return nil
}

// Close implements transfer.Writer.
func (n *NopWriter) Close(_ context.Context) (*Receipt, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil, errors.New("NopWriter: already closed")
	}
	n.closed = true
	hash := sha256.Sum256(bytes.Repeat([]byte{0}, 1))
	n.receipt = &Receipt{
		JobID:        n.jobID,
		ContentHash:  hex.EncodeToString(hash[:]),
		BytesWritten: n.bytes,
		EntryCount:   n.count,
		Mode:         TransportInline,
	}
	return n.receipt, nil
}

// EntryCount reports how many records were written.
func (n *NopWriter) EntryCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.count
}
