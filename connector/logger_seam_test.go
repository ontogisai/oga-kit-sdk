package connector

import (
	"io"
	"log/slog"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/kitlog"
)

// Property 10 (connector seam): a nil Config.Logger defaults to the
// identity-seeded kitlog.Default(); an explicit Logger is preserved unchanged.
func TestConfig_Defaults_LoggerSeam(t *testing.T) {
	c := &Config{}
	c.defaults()
	if c.Logger != kitlog.Default() {
		t.Fatalf("nil Logger should default to kitlog.Default(), got %p", c.Logger)
	}

	custom := slog.New(slog.NewTextHandler(io.Discard, nil))
	c2 := &Config{Logger: custom}
	c2.defaults()
	if c2.Logger != custom {
		t.Fatalf("explicit Logger must be preserved unchanged")
	}
}
