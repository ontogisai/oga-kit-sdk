package streampipeline

import (
	"encoding/json"
	"strings"
	"testing"
)

// tsReadSchema mirrors the kg_ts_read input schema's decision-relevant shape:
// mode/from/metric required, mode constrained to range|multi.
const tsReadSchema = `{
	"type": "object",
	"properties": {
		"mode": {"type": "string", "enum": ["range", "multi"]},
		"from": {"type": "string"},
		"metric": {"type": "string"},
		"source_id": {"type": "string"},
		"include_stats": {"type": "boolean"}
	},
	"required": ["mode", "from", "metric"]
}`

func TestValidateToolArgs(t *testing.T) {
	schema := json.RawMessage(tsReadSchema)

	tests := []struct {
		name      string
		args      map[string]any
		wantOK    bool
		wantParts []string
	}{
		{
			name:   "valid call passes",
			args:   map[string]any{"mode": "range", "from": "2026-06-29", "metric": "cop", "include_stats": true},
			wantOK: true,
		},
		{
			name:      "missing required mode and metric",
			args:      map[string]any{"from": "2026-06-29", "format": "points"},
			wantParts: []string{"missing required field(s)", "metric", "mode"},
		},
		{
			name:      "empty-string required treated as missing",
			args:      map[string]any{"mode": "", "from": "2026-06-29", "metric": "cop"},
			wantParts: []string{"missing required field(s): mode"},
		},
		{
			name:      "invalid enum value",
			args:      map[string]any{"mode": "single", "from": "2026-06-29", "metric": "cop"},
			wantParts: []string{"mode must be one of [range, multi]", "single"},
		},
		{
			name:   "false bool required is NOT missing",
			args:   map[string]any{"mode": "range", "from": "x", "metric": "y", "include_stats": false},
			wantOK: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := validateToolArgs(schema, tc.args)
			if tc.wantOK {
				if got != "" {
					t.Fatalf("expected pass, got %q", got)
				}
				return
			}
			if got == "" {
				t.Fatalf("expected a validation message, got none")
			}
			for _, p := range tc.wantParts {
				if !strings.Contains(got, p) {
					t.Errorf("message %q missing %q", got, p)
				}
			}
		})
	}
}

// TestValidateToolArgs_FailsOpen verifies absent/unparseable schemas never block.
func TestValidateToolArgs_FailsOpen(t *testing.T) {
	if got := validateToolArgs(nil, map[string]any{}); got != "" {
		t.Errorf("nil schema must fail open, got %q", got)
	}
	if got := validateToolArgs(json.RawMessage(`{bad`), map[string]any{}); got != "" {
		t.Errorf("unparseable schema must fail open, got %q", got)
	}
	// Schema with no required + no enum → nothing to enforce.
	if got := validateToolArgs(json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`), map[string]any{}); got != "" {
		t.Errorf("no-constraint schema must pass, got %q", got)
	}
}
