package loader

import (
	"context"
	"strings"
	"testing"
)

// OGA-1036: the source is a presigned GET URL. A transport failure used to come
// back as `fetch remote source: Get "<full presigned URL>": …`, which the server
// writes into its 502 body; the platform logs that body and Temporal records it.
func TestMaterializeRemoteSource_TransportErrorOmitsPresignedQuery(t *testing.T) {
	req := &LoadRequest{SourceURI: "http://127.0.0.1:1/kits/_data-import/t1/src.json?X-Amz-Credential=AKIAEXAMPLE&X-Amz-Signature=deadbeef"}
	cleanup, err := materializeRemoteSource(context.Background(), req)
	cleanup()
	if err == nil {
		t.Fatal("expected a transport error against a closed port")
	}
	for _, leak := range []string{"AKIAEXAMPLE", "deadbeef", "X-Amz-Signature"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error carries the presigned credential %q: %q", leak, err.Error())
		}
	}
	if !strings.Contains(err.Error(), "src.json?REDACTED") {
		t.Errorf("error lost the source location: %q", err.Error())
	}
}
