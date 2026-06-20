package streampipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ontogisai/oga-kit-sdk/agent"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ErrSchemaValidation indicates the LLM's structured output failed JSON Schema
// validation after the stricter retry.
var ErrSchemaValidation = errors.New("structured output failed schema validation")

// RunSync drives the pipeline, then validates the assembled artifact against
// the supplied JSON Schema and unmarshals it into T.
//
// The pipeline's grounding strategy gathers evidence and the assembly LLM call
// produces the artifact; RunSync then extracts the JSON object from the
// artifact, validates it against schema, and unmarshals into T. On validation
// failure it retries ONCE with a stricter assembly prompt that appends the
// validation error and the JSON-only instruction. If the second attempt also
// fails, it returns ErrSchemaValidation.
//
// Note: schema enforcement is post-hoc (validate + stricter retry), not yet an
// LLM-native tool_use constraint — the gateway LLM surface does not expose a
// structured tool_use contract today. The retry loop gives deterministic
// validation; native tool_use enforcement is a follow-up.
//
// T is typically a kit-defined struct or map[string]any matching the schema.
func RunSync[T any](
	ctx context.Context,
	p *Pipeline,
	deps Deps,
	input Input,
	planner Planner,
	schema *jsonschema.Schema,
) (T, []agent.CitationSource, error) {
	// Attempt 1.
	raw, citations, err := p.runArtifact(ctx, deps, jsonInput(input, schema, ""), planner)
	if err != nil {
		return zero[T](), citations, err
	}
	if parsed, perr := validateAndUnmarshal[T](raw, schema); perr == nil {
		return parsed, citations, nil
	} else if deps.Logger != nil {
		deps.Logger.WarnContext(ctx, "structured output validation failed; retrying stricter",
			"error", perr, "tenant_id", input.TenantID)
		_ = perr
	}

	// Attempt 2: stricter retry, seeding the prior (bad) output + the error.
	retryErrHint := "previous attempt produced output that failed schema validation"
	raw2, citations2, err := p.runArtifact(ctx, deps, jsonInput(input, schema, retryErrHint+": "+raw), planner)
	if err != nil {
		return zero[T](), citations2, err
	}
	if parsed, perr := validateAndUnmarshal[T](raw2, schema); perr == nil {
		return parsed, citations2, nil
	} else {
		return zero[T](), citations2, fmt.Errorf("%w: %v", ErrSchemaValidation, perr)
	}
}

// RunText drives the pipeline and returns the assembled artifact text plus the
// consolidated citations, draining the streaming events to a buffer
// (stream->collect). It is the non-streaming entry point for callers that need
// a single answer string rather than an event stream — e.g. the platform
// Knowledge Agent's synchronous message/send path, or any non-streaming channel
// (Telegram, etc.). Both this and RunSync[T] share the same underlying drain,
// so a non-streaming caller exercises the exact same ReAct loop as the
// streaming path — there is one engine, not two (OGA-419).
//
// Errors propagate from the pipeline (planning/assembly transport failures).
// An empty answer with nil error means the loop produced no artifact text
// (the caller decides how to surface that).
func RunText(
	ctx context.Context,
	p *Pipeline,
	deps Deps,
	input Input,
	planner Planner,
) (string, []agent.CitationSource, error) {
	if p == nil {
		p = NewPipeline()
	}
	return p.runArtifact(ctx, deps, input, planner)
}

// jsonInput augments the assembly prompt to instruct strict JSON-only output
// conforming to the schema. The retryHint (empty on the first attempt) appends
// the prior failure so the second attempt can self-correct.
func jsonInput(in Input, schema *jsonschema.Schema, retryHint string) Input {
	var b strings.Builder
	b.WriteString(in.AssemblyPrompt)
	b.WriteString("\n\nRespond with a SINGLE JSON object only — no prose, no markdown code fences. ")
	b.WriteString("The object MUST conform to the provided JSON Schema for this task.")
	if schema != nil {
		if loc := schema.Location; loc != "" {
			// Location is informational; the concrete schema is enforced by
			// validateAndUnmarshal after generation.
			_ = loc
		}
	}
	if retryHint != "" {
		b.WriteString("\n\nYour ")
		b.WriteString(retryHint)
		b.WriteString("\nFix the output so it validates.")
	}
	in.AssemblyPrompt = b.String()
	return in
}

// validateAndUnmarshal extracts the JSON object from raw, validates it against
// schema, and unmarshals it into T.
func validateAndUnmarshal[T any](raw string, schema *jsonschema.Schema) (T, error) {
	jsonText := extractJSONObject(raw)
	if jsonText == "" {
		return zero[T](), fmt.Errorf("no JSON object found in output")
	}

	// Validate the generic decoded value against the schema first.
	var generic any
	if err := json.Unmarshal([]byte(jsonText), &generic); err != nil {
		return zero[T](), fmt.Errorf("unmarshal generic: %w", err)
	}
	if schema != nil {
		if err := schema.Validate(generic); err != nil {
			return zero[T](), fmt.Errorf("schema validate: %w", err)
		}
	}

	// Unmarshal into the typed target.
	var out T
	if err := json.Unmarshal([]byte(jsonText), &out); err != nil {
		return zero[T](), fmt.Errorf("unmarshal typed: %w", err)
	}
	return out, nil
}

// extractJSONObject strips markdown code fences and returns the substring from
// the first '{' to the last '}'. Returns "" when no object delimiters exist.
func extractJSONObject(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return ""
	}
	return s[start : end+1]
}

// zero returns the zero value of T.
func zero[T any]() T {
	var z T
	return z
}
