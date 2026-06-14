package streampipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ontogisai/oga-kit-sdk/agent"
)

// PlaceholderResolver resolves a single placeholder token (the text between the
// braces, e.g. "entity_id" or "entity_properties.model") to a string value.
// ok=false means the token is unknown — the substitution pass leaves it
// verbatim and logs a single WARN listing all unresolved tokens.
//
// It is set on Input.Placeholders by the proactive path
// (runProactiveReasoning, via NewEventPlaceholderResolver). The reactive path
// leaves it nil — the LLMToolPlanner produces concrete arguments, not
// templated ones, so there is nothing to substitute.
type PlaceholderResolver func(token string) (string, bool)

// placeholderPattern matches {token} where token is a dotted/underscored
// identifier (entity_id, entity_type, entity_properties.model, time_minus_24h).
var placeholderPattern = regexp.MustCompile(`\{([a-zA-Z0-9_.\-]+)\}`)

// NewEventPlaceholderResolver builds a PlaceholderResolver over a proactive
// event. It resolves:
//
//	{entity_id} {entity_type} {event_type} {event_id} {severity}
//	{h3_cell} {tenant_id}                         -> the matching event field
//	{time_now}                                    -> now (RFC3339, UTC)
//	{time_minus_<dur>}                            -> now - <dur> (Go duration:
//	                                                 24h, 1h, 30m, ...)
//	{entity_properties.<key>}                     -> event.Payload[<key>]
//
// now is injected so tests are deterministic; production passes time.Now().
func NewEventPlaceholderResolver(event *agent.ProactiveEvent, now time.Time) PlaceholderResolver {
	return func(token string) (string, bool) {
		if event == nil {
			return "", false
		}
		switch token {
		case "entity_id":
			return event.EntityID, true
		case "entity_type":
			return event.EntityType, true
		case "event_type":
			return event.EventType, true
		case "event_id":
			return event.EventID, true
		case "severity":
			return event.Severity, true
		case "h3_cell":
			return event.H3Cell, true
		case "tenant_id":
			return event.TenantID, true
		case "time_now":
			return now.UTC().Format(time.RFC3339), true
		}

		if key, ok := strings.CutPrefix(token, "entity_properties."); ok {
			if v, exists := event.Payload[key]; exists {
				return stringifyScalar(v), true
			}
			return "", false
		}

		if durStr, ok := strings.CutPrefix(token, "time_minus_"); ok {
			if d, err := time.ParseDuration(durStr); err == nil {
				return now.Add(-d).UTC().Format(time.RFC3339), true
			}
			return "", false
		}

		return "", false
	}
}

// substitutePlan rewrites each step's Arguments, replacing {token} occurrences
// in string values using resolve. It never mutates the input maps — every
// step's Arguments is replaced with a freshly-allocated map. This matters
// because GroundingStrategyPlanner shares the profile's strategy argument maps
// by reference; mutating them in place would corrupt the profile for the next
// proactive event.
//
// Unknown tokens are left verbatim and collected for a single WARN log so a
// typo'd placeholder (or one referencing an empty event field that doesn't
// exist) is observable without silently dropping a tool argument.
func substitutePlan(ctx context.Context, plan *ToolPlan, resolve PlaceholderResolver, logger *slog.Logger) {
	if plan == nil || resolve == nil {
		return
	}
	unresolved := make(map[string]struct{})
	for i := range plan.Steps {
		if len(plan.Steps[i].Arguments) == 0 {
			continue
		}
		substituted, _ := substituteValue(plan.Steps[i].Arguments, resolve, unresolved).(map[string]any)
		plan.Steps[i].Arguments = substituted
	}
	if len(unresolved) > 0 {
		tokens := make([]string, 0, len(unresolved))
		for tok := range unresolved {
			tokens = append(tokens, tok)
		}
		sort.Strings(tokens)
		if logger == nil {
			logger = slog.Default()
		}
		logger.WarnContext(ctx, "streampipeline: unresolved grounding placeholders left verbatim",
			"tokens", tokens)
	}
}

// substituteValue recursively rewrites placeholder tokens in string values,
// returning new containers (never mutating the input). Non-string scalars are
// returned unchanged.
func substituteValue(v any, resolve PlaceholderResolver, unresolved map[string]struct{}) any {
	switch t := v.(type) {
	case string:
		return placeholderPattern.ReplaceAllStringFunc(t, func(match string) string {
			token := match[1 : len(match)-1]
			if val, ok := resolve(token); ok {
				return val
			}
			unresolved[token] = struct{}{}
			return match
		})
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = substituteValue(vv, resolve, unresolved)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = substituteValue(vv, resolve, unresolved)
		}
		return out
	default:
		return v
	}
}

// stringifyScalar renders an event-payload value as a string for placeholder
// substitution. Scalars (string, bool, numbers) use their natural form; JSON
// numbers arrive as float64 and integral values render without a decimal
// point. Non-scalar values (maps, slices) are JSON-encoded as a last resort so
// the substitution never panics.
func stringifyScalar(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		// JSON unmarshals all numbers to float64; render integral values
		// without a trailing ".0".
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case nil:
		return ""
	default:
		if b, err := json.Marshal(t); err == nil {
			return string(b)
		}
		return fmt.Sprintf("%v", t)
	}
}
