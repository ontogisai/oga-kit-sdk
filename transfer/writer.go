package transfer

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
)

// Writer is what kit authors hand records to. The same surface serves
// ontology loaders (WriteEntityType + WriteHierarchy) and data loaders
// (WriteVertex + WriteEdge). The writer is single-use: call
// [Writer.Close] exactly once at the end of a load, then drop the
// reference.
//
// The default implementation ([NewWriter]) keeps the body in memory up to
// [InlineBodyLimit] and commits it inline; past that it spools the body to a
// temp file and uploads it from there through a presigned URL, so memory does
// not grow with the artifact. The kit author never sees the switch — Close
// returns the same [Receipt] shape either way, with [Receipt.Mode] reporting
// which path was taken.
//
// A writer that is abandoned rather than closed should be released with
// [Discarder.Discard] when it implements it; the SDK's connector and loader
// servers do this on every path that drops a writer uncommitted.
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

	// WriteRelationshipType emits one relationship (edge) type
	// definition. Used by kind=ontology loaders that register edge
	// types dynamically (OGA-659) — symmetric with WriteEntityType.
	// The platform merges these into the active ontology's relationship
	// types (it never blindly carries them forward — OGA-564) and
	// materializes the {tenant}_RelationshipTypeDef projection.
	WriteRelationshipType(ctx context.Context, t RelationshipTypeDef) error

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

// Discarder is implemented by a [Writer] that holds resources an abandoned
// artifact must release — for the default writer, the temp file a large body
// spools to.
//
// It is a separate, optional interface rather than a method on Writer so that
// no existing Writer implementation breaks. Callers check for it with a type
// assertion on the writer they created; a wrapper that embeds Writer hides it.
type Discarder interface {
	// Discard abandons the artifact without committing it and releases what
	// the writer holds. It is idempotent and a no-op after Close, so a caller
	// may defer it unconditionally right after creating the writer: the
	// deferred call then cleans up exactly the paths that never committed.
	// After Discard, every Write* and Close returns an error.
	Discard() error
}

// DiscardWriter calls Discard on w when w implements [Discarder], and does
// nothing otherwise — a writer without it holds nothing the garbage collector
// cannot reclaim. It is what a server defers right after creating a writer:
// paths that committed have already closed it, so only the abandoned ones
// release anything.
func DiscardWriter(w Writer) error {
	if d, ok := w.(Discarder); ok {
		return d.Discard()
	}
	return nil
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

	// UpstreamRef is an OPAQUE correlation handle the submitter chooses, which
	// the platform stores with the job and echoes back verbatim on the
	// sync-outcome report and on loader.status (OGA-917).
	//
	// It exists so a connector can tie a report back to the upstream version
	// that produced it — an export timestamp, a changeset id, a content
	// revision. The platform NEVER parses, validates, interprets or branches on
	// the content: whatever bytes go in come back out.
	//
	// Bounded at [UpstreamRefMaxBytes]. Over-length is REJECTED by the platform,
	// never truncated — a silently shortened correlation handle is worse than a
	// refused submission, because it correlates to the wrong thing.
	//
	// Optional. Absent stays absent; the platform never synthesises a value. A
	// platform predating OGA-917 ignores the field, so setting it is safe
	// against an older deployment.
	//
	// One submission is one artifact, so several artifacts that belong to one
	// logical upstream version each carry the SAME UpstreamRef and produce
	// SEPARATE reports; grouping them is the kit's choice.
	UpstreamRef string `json:"upstream_ref,omitempty"`
}

// UpstreamRefMaxBytes bounds [CompleteRequest.UpstreamRef].
//
// Declared here so a kit author can check before submitting rather than
// discovering the limit as a rejected load. The platform enforces the same
// bound and refuses anything longer.
const UpstreamRefMaxBytes = 512

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

// WriterOption configures a writer at construction.
//
// The variadic is retained on NewWriter / NewDataWriter / NewOntologyWriter so an
// option can be added without breaking those three signatures. (An earlier
// WithEdgeCompleteness was removed in OGA-930 when the platform stopped reading the
// artifact's assertion — it is declared in the kit manifest now.)
type WriterOption func(*bufferedWriter)

// WithUpstreamRef sets the opaque correlation handle carried on every artifact
// this writer commits, echoed back on the sync-outcome report and on
// loader.status (OGA-917). See [CompleteRequest.UpstreamRef].
//
// Set it per upstream version, which is why it belongs on the writer rather than
// on a single Commit: a connector that spreads one version across several
// artifacts constructs one writer per version and every artifact it commits
// carries the same handle.
//
// An over-length value fails the writer at construction — every write and Close
// return the error and NOTHING is committed — rather than being refused by the
// platform after the body has been uploaded. The bound is [UpstreamRefMaxBytes],
// measured in BYTES, not runes, because that is what the platform enforces.
func WithUpstreamRef(ref string) WriterOption {
	return func(w *bufferedWriter) {
		if len(ref) > UpstreamRefMaxBytes {
			w.optErr = fmt.Errorf(
				"transfer: upstream_ref is %d bytes, limit is %d (the platform rejects rather than truncates)",
				len(ref), UpstreamRefMaxBytes)
			return
		}
		w.upstreamRef = ref
	}
}

// NewWriter constructs the default in-process writer. Production
// callers use this with an [HTTPCommitClient]; tests can substitute a
// stub via the same constructor.
//
// The writer keeps the body in memory up to [InlineBodyLimit]; past that
// it spools to an unlinked temp file in [os.TempDir] and commits through
// the presigned-upload path (see bufferedWriter). The returned writer also
// implements [Discarder]. kind controls which platform-side dispatcher
// receives the artifact; kit authors get this set automatically by
// [NewOntologyWriter] / [NewDataWriter].
func NewWriter(client CommitClient, kind LoadKind, kitID string, opts ...WriterOption) Writer {
	w := &bufferedWriter{
		client: client,
		hash:   sha256.New(),
		header: Header{
			Format:        FormatNDJSON,
			FormatVersion: FormatVersion,
			Kind:          kind,
			KitID:         kitID,
		},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(w)
		}
	}
	return w
}

// NewOntologyWriter constructs a writer pre-configured for the
// ontology dispatcher. Sugar on top of [NewWriter].
func NewOntologyWriter(client CommitClient, kitID string, opts ...WriterOption) Writer {
	return NewWriter(client, KindOntology, kitID, opts...)
}

// NewDataWriter constructs a writer pre-configured for the data
// dispatcher. Sugar on top of [NewWriter].
func NewDataWriter(client CommitClient, kitID string, opts ...WriterOption) Writer {
	return NewWriter(client, KindData, kitID, opts...)
}

// spoolFilePrefix names the temp file a large artifact spools to. The file is
// unlinked as soon as it is created, so the name is normally never seen; it
// matters only where the unlink fails (see bufferedWriter.startSpool), and it
// is searchable, so an operator who finds one left behind knows its origin.
const spoolFilePrefix = "oga-transfer-artifact-"

// spoolBufferSize is the write buffer in front of the spool file — the same
// 256 KiB the fetch package copies through. Peak memory for a spooled artifact
// is this plus one encoded record, not the artifact.
const spoolBufferSize = 256 << 10

// bufferedWriter is the default Writer.
//
// Every byte of the body — the lazy header and each envelope line — goes
// through appendBody, which feeds a running SHA-256, so Close has the
// content hash without reading the body a second time.
//
// The body stays in memory while it fits inline (at most [InlineBodyLimit]),
// and Close commits those bytes inline, byte-for-byte what earlier SDK
// versions produced. The first record that would take it past the limit moves
// it to a temp file (startSpool) and every later record is written there
// through a buffered writer; Close then uploads the file through the presigned
// path, so memory stays at the copy buffer however large the artifact grows.
//
// The temp file is unlinked the moment it is created and only its descriptor
// is kept. Nothing therefore needs to delete it: it disappears when the
// descriptor is closed — by Close on every outcome, by Discard, or, for a
// writer that is simply dropped, when the garbage collector finalizes the
// *os.File — and also when the process dies, SIGKILL included, which no
// cleanup code could cover.
type bufferedWriter struct {
	client CommitClient
	header Header
	count  int
	closed bool
	// discarded records that Discard, not Close, ended the writer, so later
	// calls can say which.
	discarded bool
	// upstreamRef is the opaque correlation handle set by WithUpstreamRef and
	// carried on every CompleteRequest this writer commits. Empty ⇒ omitted.
	upstreamRef string
	// optErr holds a construction-time option failure. NewWriter returns
	// a bare Writer, so there is nowhere to surface it at construction;
	// it is returned from every write and from Close instead, which is
	// what stops a mis-configured writer from committing an artifact.
	optErr error

	// hash is fed every body byte as it is written; size counts them.
	hash hash.Hash
	size int64
	// line is reused scratch for encoding one record.
	line bytes.Buffer
	// buf holds the body while it fits inline. It is released, and stays
	// empty, once the body moves to spool.
	buf bytes.Buffer
	// spool, once non-nil, holds the whole body; spoolW buffers writes to it.
	// spoolPath is set only when the immediate unlink failed, and names the
	// file releaseBody must remove.
	spool     *os.File
	spoolW    *bufio.Writer
	spoolPath string
	// writeErr is the first spool I/O failure. It is sticky: once the spool
	// may be missing bytes, no later write or Close may pretend otherwise.
	writeErr error
}

var _ Discarder = (*bufferedWriter)(nil)

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

func (w *bufferedWriter) WriteRelationshipType(_ context.Context, t RelationshipTypeDef) error {
	if t.Name == "" {
		return errors.New("transfer.WriteRelationshipType: name is required")
	}
	return w.writeEnvelope(EntryRelationshipType, t)
}

func (w *bufferedWriter) WriteHierarchy(_ context.Context, h HierarchyEntry) error {
	if h.TypeName == "" || h.ParentType == "" {
		return errors.New("transfer.WriteHierarchy: type_name and parent_type are both required")
	}
	return w.writeEnvelope(EntryHierarchy, h)
}

func (w *bufferedWriter) writeEnvelope(kind EntryKind, value any) error {
	if w.optErr != nil {
		return w.optErr
	}
	if w.discarded {
		return errors.New("transfer.Writer: cannot write after Discard")
	}
	if w.closed {
		return errors.New("transfer.Writer: cannot write after Close")
	}
	if w.writeErr != nil {
		return w.writeErr
	}
	if w.count == 0 {
		// Lazy header — written once on first record so empty Close()
		// produces an empty artifact rather than a header-only blob.
		if err := w.appendJSONLine(w.header); err != nil {
			return fmt.Errorf("write artifact header: %w", err)
		}
	}
	if err := w.appendJSONLine(Envelope{Kind: kind, Value: value}); err != nil {
		return fmt.Errorf("write %s envelope: %w", kind, err)
	}
	w.count++
	return nil
}

// appendJSONLine encodes v as a single line of NDJSON and appends it to the
// body. json.Encoder appends a newline already; we don't add a second one. The
// whole line is encoded before any of it is appended, so a value that fails to
// marshal leaves the body untouched.
func (w *bufferedWriter) appendJSONLine(v any) error {
	w.line.Reset()
	enc := json.NewEncoder(&w.line)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	return w.appendBody(w.line.Bytes())
}

// appendBody adds p to the body, wherever the body currently lives, and to the
// running hash. It is the only place body bytes are written.
func (w *bufferedWriter) appendBody(p []byte) error {
	if w.spool == nil && w.size+int64(len(p)) > InlineBodyLimit {
		if err := w.startSpool(); err != nil {
			w.writeErr = err
			return err
		}
	}
	if w.spool != nil {
		if _, err := w.spoolW.Write(p); err != nil {
			w.writeErr = fmt.Errorf("transfer: write artifact spool: %w", err)
			return w.writeErr
		}
	} else {
		w.buf.Write(p)
	}
	w.hash.Write(p)
	w.size += int64(len(p))
	return nil
}

// startSpool moves the body from memory to an unlinked temp file. It runs once,
// when the next line would take the body past InlineBodyLimit.
//
// The file is removed by name immediately and only the descriptor is kept, so
// it can never be left behind — see bufferedWriter. Where removing an open file
// fails (Windows), the path is kept instead and releaseBody deletes it.
//
// Failing to create the file is an error the caller sees, naming the
// directory: on a read-only root filesystem the fix is a writable TMPDIR, and
// guessing at an alternative would hide that.
func (w *bufferedWriter) startSpool() error {
	f, err := os.CreateTemp("", spoolFilePrefix+"*")
	if err != nil {
		return fmt.Errorf("transfer: artifact exceeds %d bytes and must spool to a temp file, but none can be created in %q (set TMPDIR to a writable directory): %w",
			InlineBodyLimit, os.TempDir(), err)
	}
	if rmErr := os.Remove(f.Name()); rmErr != nil {
		w.spoolPath = f.Name()
	}
	w.spool = f
	w.spoolW = bufio.NewWriterSize(f, spoolBufferSize)
	if _, err := w.spoolW.Write(w.buf.Bytes()); err != nil {
		return fmt.Errorf("transfer: write artifact spool: %w", err)
	}
	w.buf = bytes.Buffer{} // release the in-memory copy
	return nil
}

// releaseBody frees whatever holds the body: the in-memory buffer, or the spool
// descriptor (deleting the file by name only when the early unlink failed).
// Safe to call more than once.
func (w *bufferedWriter) releaseBody() error {
	w.buf = bytes.Buffer{}
	w.line = bytes.Buffer{}
	if w.spool == nil {
		return nil
	}
	f, path := w.spool, w.spoolPath
	w.spool, w.spoolW, w.spoolPath = nil, nil, ""
	err := f.Close()
	if path != "" {
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			err = errors.Join(err, rmErr)
		}
	}
	if err != nil {
		return fmt.Errorf("transfer: release artifact spool: %w", err)
	}
	return nil
}

// Discard implements [Discarder]: it abandons the artifact, commits nothing and
// releases the body. Idempotent, and a no-op after Close.
func (w *bufferedWriter) Discard() error {
	if w.closed {
		return nil
	}
	w.closed = true
	w.discarded = true
	return w.releaseBody()
}

func (w *bufferedWriter) Close(ctx context.Context) (*Receipt, error) {
	if w.optErr != nil {
		// A WriterOption was rejected at construction, so refuse to commit rather
		// than shipping an artifact that does not carry what the caller asked for.
		// Marked closed so a caller that ignores the error cannot retry into a
		// commit.
		//
		// [WithUpstreamRef] is the only producer of optErr today: an over-length
		// correlation handle fails here rather than after the body has been
		// uploaded, because the platform rejects it too and truncating a
		// correlation handle is worse than refusing one. (The mechanism predates
		// it — WithEdgeCompleteness used it before OGA-930 moved that assertion
		// into the kit manifest.)
		w.closed = true
		_ = w.releaseBody()
		return nil, w.optErr
	}
	if w.discarded {
		return nil, errors.New("transfer.Writer: already discarded")
	}
	if w.closed {
		return nil, errors.New("transfer.Writer: already closed")
	}
	w.closed = true

	receipt, err := w.commit(ctx)
	// The body is released on every outcome. A release failure is reported only
	// when the commit failed too: once the platform has accepted the artifact,
	// an error from Close would read as "not committed" and invite a retry that
	// commits it twice. (Closing an unlinked file's descriptor does not fail in
	// practice; the case exists for the Windows fallback path.)
	if relErr := w.releaseBody(); relErr != nil && err != nil {
		err = errors.Join(err, relErr)
	}
	return receipt, err
}

// commit sends the body to the platform: inline while it never left memory,
// presigned from the spool otherwise.
func (w *bufferedWriter) commit(ctx context.Context) (*Receipt, error) {
	if w.writeErr != nil {
		return nil, fmt.Errorf("transfer: artifact not committed: %w", w.writeErr)
	}
	hashHex := hex.EncodeToString(w.hash.Sum(nil))
	if w.spool == nil {
		return w.commitInline(ctx, w.buf.Bytes(), hashHex, w.size)
	}
	if err := w.spoolW.Flush(); err != nil {
		return nil, fmt.Errorf("transfer: artifact not committed: flush artifact spool: %w", err)
	}
	return w.commitPresigned(ctx, io.NewSectionReader(w.spool, 0, w.size), hashHex, w.size)
}

func (w *bufferedWriter) commitInline(ctx context.Context, body []byte, hashHex string, size int64) (*Receipt, error) {
	resp, err := w.client.Complete(ctx, &CompleteRequest{
		InlineBody:  body,
		Kind:        w.header.Kind,
		Format:      FormatNDJSON,
		ContentHash: hashHex,
		EntryCount:  w.count,
		UpstreamRef: w.upstreamRef,
	})
	if err != nil {
		return nil, fmt.Errorf("loader.complete (inline): %w", err)
	}
	return w.receipt(resp, hashHex, size, TransportInline), nil
}

func (w *bufferedWriter) commitPresigned(ctx context.Context, body io.Reader, hashHex string, size int64) (*Receipt, error) {
	prep, err := w.client.PrepareUpload(ctx)
	if err != nil {
		return nil, fmt.Errorf("loader.prepare_upload: %w", err)
	}
	if prep.UploadURL == "" || prep.UploadToken == "" {
		return nil, errors.New("loader.prepare_upload: response missing upload_url or upload_token")
	}
	if err := w.client.PutBytes(ctx, prep.UploadURL, body, size); err != nil {
		return nil, fmt.Errorf("upload artifact body: %w", err)
	}
	resp, err := w.client.Complete(ctx, &CompleteRequest{
		UploadToken: prep.UploadToken,
		Kind:        w.header.Kind,
		Format:      FormatNDJSON,
		ContentHash: hashHex,
		EntryCount:  w.count,
		UpstreamRef: w.upstreamRef,
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
