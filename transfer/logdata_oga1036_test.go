package transfer

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// OGA-1036: a network failure on the presigned PUT returned a *url.Error whose
// text is the full presigned URL, i.e. a working credential, and the connector
// and loader servers log that error.
func TestPutBytes_TransportErrorOmitsPresignedQuery(t *testing.T) {
	const presigned = "http://127.0.0.1:1/loader-uploads/t1/abc.ndjson?X-Amz-Credential=AKIAEXAMPLE&X-Amz-Signature=deadbeef&X-Amz-Security-Token=tok"
	c := &HTTPCommitClient{httpClient: &http.Client{Timeout: 2 * time.Second}}
	err := c.PutBytes(context.Background(), presigned, bytes.NewReader([]byte("x")), 1)
	if err == nil {
		t.Fatal("expected a transport error against a closed port")
	}
	for _, leak := range []string{"AKIAEXAMPLE", "deadbeef", "X-Amz-Signature", "tok"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error carries the presigned credential %q: %q", leak, err.Error())
		}
	}
	if !strings.Contains(err.Error(), "loader-uploads/t1/abc.ndjson?REDACTED") {
		t.Errorf("error lost the object location: %q", err.Error())
	}
}
