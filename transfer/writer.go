package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Writer is what kit authors hand records to. The same surface serves
// ontology loaders (WriteEntityType + WriteHierarchy) and data loaders
// (WriteVertex + WriteEdge). The writer is single-use: call
// [Writer.Close] exactly once at the end of a load, then drop the
// reference.
//
// Implementations buffer in memory up to [InlineBodyLimit]; above that
// they stream to a presigned-upload destination. The kit author never
// sees the switch — Close returns the same [Receipt] shape either way,
// with [Receipt.Mode] reporting which path was taken.
//
// Writer is NOT safe for concurrent use by multiple goroutines. A kit
// that wants to fan out parsing should buffer per-goroutine and call
// the writer from a single drain goroutine.
type Writer interface {
	// WriteVertex emits one vertex record into the artifact body.
	WriteVertex(ctx context.Context, v Vertex) error

	// WriteEdge emits one edge record into the artifact body.
	WriteEdge(ctx context.Context, e Edge) error

	// WriteEntityType emits one entity-type definition.
	WriteEntityType(ctx context.Context, t EntityTypeDef) error

	// WriteHierarchy emits one parent-child relationship between two
	// entity types. Send these only after the corresponding
	// EntityTypeDef entries.
	WriteHierarchy(ctx context.Context, h HierarchyEntry) error

	// Close finalizes the artifact and commits it to the platform.
	// Returns a [Receipt] with the platform-issued job_id. Once Close
	// returns successfully, the platform has accepted the artifact;
	// downstream processing happens asynchronously and the kit's
	// caller polls loader.status to track it. After Close, the
	// writer is unusable — calling any Write* method returns an
	// error.
	Close(ctx context.Context) (*Receipt, error)
}

// CommitClient is the small interface the writer uses to talk to the
// platform. The HTTP-backed implementation in [HTTPCommitClient]
// covers production use; tests provide a fake to capture commit
// payloads without spinning up a real gateway.
type CommitClient interface {
	// PrepareUpload calls loader.prepare_upload. Returns a presigned
	// PUT URL plus an opaque upload_token that the writer feeds back
	// to Complete after streaming the body.
	PrepareUpload(ctx context.Context) (*PrepareUploadResponse, error)

	// PutBytes streams the body to the presigned URL. The
	// implementation is just an HTTP PUT; it lives on the client so
	// tests can inject a fake transport.
	PutBytes(ctx context.Context, uploadURL string, body io.Reader, size int64) error

	// Complete calls loader.complete. Pass exactly one of
	// uploadToken or inlineBody — the writer enforces this on the
	// caller's behalf based on accumulated body size.
	Complete(ctx context.Context, req *CompleteRequest) (*CompleteResponse, error)
}

// PrepareUploadResponse is the decoded body of loader.prepare_upload.
type PrepareUploadResponse struct {
	// UploadURL is the presigned PUT URL the writer streams the
	// artifact body to. Short-lived (15 min by default).
	UploadURL string `json:"upload_url"`

	// UploadToken is the opaque platform-issued reference the writer
	// passes back through loader.complete after the upload succeeds.
	UploadToken string `json:"upload_token"`

	// ExpiresAt is when UploadURL stops working. Informational.
	ExpiresAt string `json:"expires_at,omitempty"`
}

// CompleteRequest is the body of loader.complete. Exactly one of
// UploadToken or InlineBody must be set.
type CompleteRequest struct {
	// UploadToken references a prior loader.prepare_upload call that
	// the writer has finished streaming bytes to. Mutually exclusive
	// with InlineBody.
	UploadToken string `json:"upload_token,omitempty"`

	// InlineBody is the raw artifact bytes for payloads under
	// [InlineBodyLimit]. Mutually exclusive with UploadToken. The
	// platform decodes the same NDJSON format whether the body
	// arrived inline or via presigned upload.
	InlineBody []byte `json:"inline_body,omitempty"`

	// Kind tells the platform which processor to dispatch to.
	Kind LoadKind `json:"kind"`

	// Format is "ndjson" today.
	Format string `json:"format"`

	// ContentHash is sha256(body) hex-encoded. The platform validates
	// the uploaded / inlined bytes match this hash; mismatch is a
	// terminal failure (rejected synchronously).
	ContentHash string `json:"content_hash"`

	// EntryCount is the record count in the body, included for the
	// platform's audit trail.
	EntryCount int `json:"entry_count"`
}

// CompleteResponse is the decoded body of loader.complete.
type CompleteResponse struct {
	// JobID is the platform-issued identifier the install / import
	// workflow polls via loader.status.
	JobID string `json:"job_id"`

	// Status is "running" or "queued" — both indicate the platform
	// accepted the artifact. Terminal status comes from
	// loader.status, not loader.complete.
	Status string `json:"status"`

	// AcceptedAt is the platform time the artifact was accepted.
	AcceptedAt string `json:"accepted_at,omitempty"`
}

// NewWriter constructs the default in-process writer. Production
// callers use this with an [HTTPCommitClient]; tests can substitute a
// stub via the same constructor.
//
// The writer buffers up to [InlineBodyLimit] in memory before
// switching to the presigned-upload path. kind controls which
// platform-side dispatcher receives the artifact; kit authors get
// this set automatically by [NewOntologyWriter] / [NewDataWriter].
func NewWriter(client CommitClient, kind LoadKind, kitID string) Writer {
	return &bufferedWriter{
		client: client,
		header: Header{
			Format:        FormatNDJSON,
			FormatVersion: FormatVersion,
			Kind:          kind,
			KitID:         kitID,
		},
	}
}

// NewOntologyWriter constructs a writer pre-configured for the
// ontology dispatcher. Sugar on top of [NewWriter].
func NewOntologyWriter(client CommitClient, kitID string) Writer {
	return NewWriter(client, KindOntology, kitID)
}

// NewDataWriter constructs a writer pre-configured for the data
// dispatcher. Sugar on top of [NewWriter].
func NewDataWriter(client CommitClient, kitID string) Writer {
	return NewWriter(client, KindData, kitID)
}

// bufferedWriter is the default Writer. It accumulates lines in an
// in-memory buffer and chooses inline vs presigned at Close time
// based on the buffer's final size. A future implementation may
// switch to streaming mid-flight when the buffer fills (so very
// large artifacts don't have to live entirely in memory before the
// upload starts), but the current shape is simpler and covers every
// known kit use case.
type bufferedWriter struct {
	client CommitClient
	header Header
	buf    bytes.Buffer
	count  int
	closed bool
}

func (w *bufferedWriter) WriteVertex(_ context.Context, v Vertex) error {
	if v.EntityType == "" {
		return errors.New("transfer.WriteVertex: entity_type is required")
	}
	return w.writeEnvelope(EntryVertex, v)
}

func (w *bufferedWriter) WriteEdge(_ context.Context, e Edge) error {
	if e.RelationshipType == "" || e.SourceID == "" || e.TargetID == "" {
		return errors.New("transfer.WriteEdge: relationship_type, source_id, target_id are all required")
	}
	return w.writeEnvelope(EntryEdge, e)
}

func (w *bufferedWriter) WriteEntityType(_ context.Context, t EntityTypeDef) error {
	if t.Name == "" {
		return errors.New("transfer.WriteEntityType: name is required")
	}
	return w.writeEnvelope(EntryEntityType, t)
}

func (w *bufferedWriter) WriteHierarchy(_ context.Context, h HierarchyEntry) error {
	if h.TypeName == "" || h.ParentType == "" {
		return errors.New("transfer.WriteHierarchy: type_name and parent_type are both required")
	}
	return w.writeEnvelope(EntryHierarchy, h)
}

func (w *bufferedWriter) writeEnvelope(kind EntryKind, value any) error {
	if w.closed {
		return errors.New("transfer.Writer: cannot write after Close")
	}
	if w.count == 0 {
		// Lazy header — written once on first record so empty Close()
		// produces an empty artifact rather than a header-only blob.
		if err := writeJSONLine(&w.buf, w.header); err != nil {
			return fmt.Errorf("write artifact header: %w", err)
		}
	}
	if err := writeJSONLine(&w.buf, Envelope{Kind: kind, Value: value}); err != nil {
		return fmt.Errorf("write %s envelope: %w", kind, err)
	}
	w.count++
	return nil
}

// writeJSONLine encodes v to the buffer as a single line of NDJSON.
// json.Encoder appends a newline already; we don't add a second one.
func writeJSONLine(buf *bytes.Buffer, v any) error {
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func (w *bufferedWriter) Close(ctx context.Context) (*Receipt, error) {
	if w.closed {
		return nil, errors.New("transfer.Writer: already closed")
	}
	w.closed = true

	body := w.buf.Bytes()
	hash := sha256.Sum256(body)
	hashHex := hex.EncodeToString(hash[:])
	size := int64(len(body))

	if size <= InlineBodyLimit {
		return w.commitInline(ctx, body, hashHex, size)
	}
	return w.commitPresigned(ctx, body, hashHex, size)
}

func (w *bufferedWriter) commitInline(ctx context.Context, body []byte, hashHex string, size int64) (*Receipt, error) {
	resp, err := w.client.Complete(ctx, &CompleteRequest{
		InlineBody:  body,
		Kind:        w.header.Kind,
		Format:      FormatNDJSON,
		ContentHash: hashHex,
		EntryCount:  w.count,
	})
	if err != nil {
		return nil, fmt.Errorf("loader.complete (inline): %w", err)
	}
	return w.receipt(resp, hashHex, size, TransportInline), nil
}

func (w *bufferedWriter) commitPresigned(ctx context.Context, body []byte, hashHex string, size int64) (*Receipt, error) {
	prep, err := w.client.PrepareUpload(ctx)
	if err != nil {
		return nil, fmt.Errorf("loader.prepare_upload: %w", err)
	}
	if prep.UploadURL == "" || prep.UploadToken == "" {
		return nil, errors.New("loader.prepare_upload: response missing upload_url or upload_token")
	}
	if err := w.client.PutBytes(ctx, prep.UploadURL, bytes.NewReader(body), size); err != nil {
		return nil, fmt.Errorf("upload artifact body: %w", err)
	}
	resp, err := w.client.Complete(ctx, &CompleteRequest{
		UploadToken: prep.UploadToken,
		Kind:        w.header.Kind,
		Format:      FormatNDJSON,
		ContentHash: hashHex,
		EntryCount:  w.count,
	})
	if err != nil {
		return nil, fmt.Errorf("loader.complete (presigned): %w", err)
	}
	return w.receipt(resp, hashHex, size, TransportPresigned), nil
}

func (w *bufferedWriter) receipt(resp *CompleteResponse, hashHex string, size int64, mode TransportMode) *Receipt {
	return &Receipt{
		JobID:        resp.JobID,
		ContentHash:  hashHex,
		BytesWritten: size,
		EntryCount:   w.count,
		Mode:         mode,
		// AcceptedAt left zero unless the platform reports it; the
		// write path doesn't need to convert the string here.
	}
}
