// Package redact turns URLs and HTTP error bodies into forms that are safe to
// put into an error or a log line.
//
// Every kit sidecar runs this SDK, and its errors travel: into the sidecar's
// logs, into HTTP error responses the platform logs, and into Temporal history
// the operator console shows. Two things must never ride along (OGA-1036):
//
//   - a presigned URL's query string, which IS the credential
//     (X-Amz-Signature, X-Amz-Credential, X-Amz-Security-Token);
//   - an upstream error body, which can echo the request. An S3 signature
//     error, for one, returns the canonical request, security token included.
package redact

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// URLLocation is raw without userinfo, query or fragment. A query that existed
// is shown as REDACTED, so "no query" and "query removed" stay distinct when
// debugging a signed URL. A string that does not parse gives "<url>".
func URLLocation(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<url>"
	}
	c := *u
	c.User = nil
	c.Fragment = ""
	c.RawFragment = ""
	if c.RawQuery != "" || c.ForceQuery {
		c.RawQuery = "REDACTED"
		c.ForceQuery = false
	}
	return c.String()
}

// ScrubURLError removes raw from err's text, leaving URLLocation(raw). A
// transport error's text is `<Op> "<URL>": <cause>`, so without this every
// network failure on a signed URL prints the signature.
//
// A *url.Error in the chain has its URL replaced. Because fmt.Errorf formats its
// message when it is called, an error that already wrapped the *url.Error keeps
// the URL in its own text; that case returns a wrapper with the text rewritten.
// Either way errors.Is / errors.As on the cause keep working.
func ScrubURLError(err error, raw string) error {
	if err == nil || raw == "" {
		return err
	}
	loc := URLLocation(raw)
	if uerr, ok := errors.AsType[*url.Error](err); ok {
		uerr.URL = loc
	}
	if msg := err.Error(); strings.Contains(msg, raw) {
		return &scrubbedError{msg: strings.ReplaceAll(msg, raw, loc), err: err}
	}
	return err
}

type scrubbedError struct {
	msg string
	err error
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.err }

var (
	xmlCode      = regexp.MustCompile(`<Code>([^<]{1,128})</Code>`)
	xmlRequestID = regexp.MustCompile(`<RequestId>([^<]{1,128})</RequestId>`)
)

// ErrorBody summarizes an upstream error body for an error message.
//
// An S3-style XML error (AWS, MinIO) is reduced to its <Code> and <RequestId>:
// the rest of such a body can carry the canonical request, the provided
// signature, the security token and the object key. Anything else is trimmed
// and capped at max bytes on a rune boundary.
func ErrorBody(b []byte, max int) string {
	s := strings.TrimSpace(string(b))
	if strings.Contains(s, "<Error>") {
		if m := xmlCode.FindStringSubmatch(s); m != nil {
			out := "upstream error " + m[1]
			if r := xmlRequestID.FindStringSubmatch(s); r != nil {
				out += " (request id " + r[1] + ")"
			}
			return out
		}
		return "upstream XML error"
	}
	return Truncate(s, max)
}

// Truncate caps s at max bytes without splitting a UTF-8 sequence and notes
// how many bytes were dropped.
func Truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…(truncated)"
}
