package redact

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestURLLocation(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://b.s3.amazonaws.com/k/a.ndjson?X-Amz-Signature=abc&X-Amz-Security-Token=tok", "https://b.s3.amazonaws.com/k/a.ndjson?REDACTED"},
		{"https://user:pass@host/p", "https://host/p"},
		{"https://host/p", "https://host/p"},
		{"http://[::1", "<url>"},
	}
	for _, tc := range cases {
		if got := URLLocation(tc.in); got != tc.want {
			t.Errorf("URLLocation(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestScrubURLError_Direct(t *testing.T) {
	const raw = "https://b.s3.amazonaws.com/k?X-Amz-Signature=deadbeef"
	inner := &url.Error{Op: "Put", URL: raw, Err: errors.New("i/o timeout")}
	got := ScrubURLError(inner, raw)
	if strings.Contains(got.Error(), "deadbeef") {
		t.Fatalf("signature survived: %q", got)
	}
	if ue, ok := errors.AsType[*url.Error](got); !ok || ue.Err.Error() != "i/o timeout" {
		t.Errorf("lost the *url.Error cause: %v", got)
	}
}

// fmt.Errorf formats eagerly, so an error that already wrapped the *url.Error
// still carries the URL in its own text.
func TestScrubURLError_Wrapped(t *testing.T) {
	const raw = "https://b.s3.amazonaws.com/k?X-Amz-Signature=deadbeef"
	err := fmt.Errorf("fetch: %w", &url.Error{Op: "Get", URL: raw, Err: errors.New("connection reset by peer")})
	got := ScrubURLError(err, raw).Error()
	if strings.Contains(got, "deadbeef") {
		t.Fatalf("signature survived: %q", got)
	}
	if !strings.Contains(got, "connection reset by peer") || !strings.Contains(got, "?REDACTED") {
		t.Errorf("lost the cause or the location: %q", got)
	}
}

// The body S3 returns for a signature mismatch (AWS's documented shape): every
// field after <Message> is either a credential fragment or the request itself.
func TestErrorBody_S3SignatureMismatch(t *testing.T) {
	body := `<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>SignatureDoesNotMatch</Code><Message>The request signature we calculated does not match the signature you provided.</Message><AWSAccessKeyId>ASIAEXAMPLE</AWSAccessKeyId><StringToSign>AWS4-HMAC-SHA256 x</StringToSign><SignatureProvided>deadbeef</SignatureProvided><CanonicalRequest>PUT /k X-Amz-Security-Token=SESSIONTOKEN</CanonicalRequest><RequestId>REQ123</RequestId></Error>`
	got := ErrorBody([]byte(body), 512)
	for _, leak := range []string{"ASIAEXAMPLE", "deadbeef", "SESSIONTOKEN", "CanonicalRequest"} {
		if strings.Contains(got, leak) {
			t.Errorf("kept %q: %q", leak, got)
		}
	}
	if got != "upstream error SignatureDoesNotMatch (request id REQ123)" {
		t.Errorf("got %q", got)
	}
}

func TestErrorBody_PlainIsCappedOnRuneBoundary(t *testing.T) {
	s := strings.Repeat("é", 300) // 2 bytes each
	got := ErrorBody([]byte(s), 101)
	if !strings.HasSuffix(got, "…(truncated)") {
		t.Fatalf("not truncated: %d bytes", len(got))
	}
	if strings.ContainsRune(strings.TrimSuffix(got, "…(truncated)"), '\uFFFD') {
		t.Errorf("split a rune")
	}
	if ErrorBody([]byte("  short  "), 512) != "short" {
		t.Errorf("short body not trimmed")
	}
}
