// Package agent — single-step ReAct decision call (OGA-419).
//
// RequestNextStep asks the LLM for ONE next action given the full observation
// transcript, replacing the plan-once RequestPlan on the interleaved path. The
// model returns either an action to take or a signal that it has enough
// evidence to answer. This is the core of the agentic reason -> act -> observe
// loop driven by streampipeline.Pipeline.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ontogisai/oga-kit-sdk/gateway"
)

// NextStepObservation is one prior tool execution in the ReAct transcript that
// the planner reasons over when choosing the next action.
type NextStepObservation struct {
	ToolName  string
	Arguments map[string]any
	Success   bool
	// Content is a bounded view of the tool result (callers truncate before
	// passing it in to keep the prompt within budget).
	Content string
	Error   string
	Skipped bool
}

// GroundingHint is the planner-facing advisory form of a kit GroundingStep. It
// is rendered into the decision prompt so the LLM knows which tools the kit
// author recommends and in what rough order — without being forced to follow
// them. It mirrors the fields of GroundingStep that matter to the planner.
type GroundingHint struct {
	Tool            string
	SuggestedArgs   map[string]any
	Rationale       string
	StronglyAdvised bool
}

// ToolSchema is a richer descriptor for a palette tool: its name, a short
// description, and (optionally) its JSON-Schema input contract. When supplied
// in a NextStepRequest, the decision prompt renders the tool's argument names +
// types so the model emits correct arguments instead of guessing (OGA-419 G1).
// The platform Knowledge Agent populates these from discovered MCP tools; a
// delegation capability (ask_knowledge_agent) supplies Name + Description with
// no InputSchema.
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	// Mutates marks whether the tool changes state (writes). When set, the
	// reactive pipeline enforces confirm-before-write for it (OGA-446). nil =
	// unknown; callers fall back to a name heuristic (see streampipeline).
	Mutates *bool `json:"mutates,omitempty"`
}

// NextStepRequest is the input to a single ReAct decision call.
type NextStepRequest struct {
	// SystemPrompt is the agent persona/instructions (PlannerPersona.SystemPrompt).
	SystemPrompt string
	// Tools is the available tool palette — bounds what the model may pick.
	Tools []string
	// ToolSchemas optionally carries richer per-tool detail (description +
	// JSON-Schema inputs) rendered into the decision prompt. Tools listed here
	// but absent from Tools are still added to the palette; tools in Tools
	// without a matching schema render as bare names. Empty → names-only.
	ToolSchemas []ToolSchema
	// Query is the user/seed query.
	Query string
	// SeedFacts is resolved factual context (event facts / investigation context).
	SeedFacts string
	// Hints are advisory grounding hints (may be empty).
	Hints []GroundingHint
	// History is the observation transcript so far, in order.
	History []NextStepObservation
	// AllowClarification gates the reactive-only `clarify` outcome (OGA-446).
	// When true, the clarify contract is appended to the decision prompt so the
	// model may pause to ask the user. The proactive path leaves it false, so its
	// decision contract is byte-identical to pre-OGA-446 (Property 1).
	AllowClarification bool
}

// NextStepDecision is the parsed single-action decision.
type NextStepDecision struct {
	// Thought is the model's first-person reasoning for this turn.
	Thought string
	// Final is true when the model judges it has enough evidence to answer; in
	// that case ToolName is empty.
	Final bool
	// ToolName is the chosen tool (empty iff Final).
	ToolName string
	// Arguments is the parameter map for the chosen tool.
	Arguments map[string]any
	// Usage is the token usage for this decision call, summing the initial and
	// (when triggered) the corrective-retry completion (OGA-420). Zero counts.
	Usage TokenUsage
	// UsageAvailable is true when the proxy reported usage for at least one of
	// the decision completions. False → Usage is zero and must not be read as a
	// real "0 tokens".
	UsageAvailable bool
	// Clarification is set when the model chose the reactive `clarify` outcome
	// (OGA-446): pause and ask the user. When set, ToolName is empty and Final is
	// false — the pipeline emits an input-required turn and executes no tool.
	Clarification *ClarificationPayload
}

// nextStepWire is the JSON shape the model is asked to produce.
type nextStepWire struct {
	Thought string `json:"thought"`
	Final   bool   `json:"final"`
	Action  *struct {
		Tool      string         `json:"tool"`
		Arguments map[string]any `json:"arguments"`
	} `json:"action"`
	// Clarify is the reactive-only third outcome (OGA-446): pause and ask the
	// user. Only honored when the request enabled AllowClarification.
	Clarify *struct {
		Question         string          `json:"question"`
		Kind             string          `json:"kind"`
		Missing          []string        `json:"missing"`
		Options          []ClarifyOption `json:"options"`
		PendingTool      string          `json:"pending_tool"`
		PartialArguments map[string]any  `json:"partial_arguments"`
	} `json:"clarify"`
}

// nextStepContract is appended to the system prompt to pin the output format.
const nextStepContract = `

You are reasoning step by step, one action at a time. You have already observed
the results of any prior actions (shown below). Decide the SINGLE next action.

Rules:
- Look at the observations so far. If a prior result is empty, the wrong type,
  or low-confidence, do NOT build on it — refine your arguments or choose a
  different tool.
- Treat any "Recommended investigation steps" as guidance, not a fixed script:
  skip, reorder, or substitute based on what you observe.
- Only call tools from the provided tool list.
- When you have gathered enough evidence to answer, stop.

Respond with a SINGLE JSON object only (no prose, no markdown fences), one of:
  {"thought": "<why this action>", "action": {"tool": "<tool_name>", "arguments": {<args>}}}
  {"thought": "<why you have enough>", "final": true}`

// nextStepClarifyContract is appended to the decision prompt ONLY when the
// request enables AllowClarification (reactive path, OGA-446). It adds the third
// outcome and instructs WHEN to use it. The proactive path never appends this,
// so its contract is byte-identical to pre-OGA-446 (Property 1).
const nextStepClarifyContract = `

You may ALSO pause to ask the user when you genuinely cannot proceed safely on
your own. Use this when: the target entity is AMBIGUOUS (your search returned
more than one plausible match), a REQUIRED argument is unknown and not derivable
from the observations, or you are about to perform a STATE-CHANGING action (a
create/update/delete) — in that last case ALWAYS confirm the exact action and its
arguments with the user before doing it. Prefer best-effort retrieval for
read-only lookups; reserve this for ambiguous targets and before any write.
Ask ONE focused question. Add a third allowed JSON object:
  {"thought": "<why you must ask>", "clarify": {"question": "<one question>",
   "kind": "disambiguation|missing_field|confirmation",
   "missing": ["<field>"], "options": [{"id": "<id>", "label": "<label>"}],
   "pending_tool": "<tool you will call once answered>",
   "partial_arguments": {<args gathered so far>}}}`

// nextStepCorrection is the corrective turn used when the first reply does not
// parse as the required JSON object.
const nextStepCorrection = `Your previous reply was not a single valid JSON object in the required shape.
Respond with ONLY one JSON object, either
{"thought": "...", "action": {"tool": "...", "arguments": {...}}} or
{"thought": "...", "final": true}. No prose, no markdown.`

// RequestNextStep issues one ReAct decision call and returns the parsed
// decision. It is self-correcting: on a parse failure it retries once with a
// corrective turn (mirrors RequestPlan / OGA-387). Transport errors are
// returned without a corrective retry.
func RequestNextStep(
	ctx context.Context,
	gw GatewayClient,
	req NextStepRequest,
	cfg PlannerConfig,
) (*NextStepDecision, error) {
	system := req.SystemPrompt + renderToolPalette(req.Tools, req.ToolSchemas) + renderHints(req.Hints) + nextStepContract
	if req.AllowClarification {
		system += nextStepClarifyContract
	}
	user := renderNextStepUser(req)

	messages := []gateway.ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}

	// Usage accumulator for this decision turn — sums the initial completion and
	// the corrective-retry completion when one is needed (OGA-420).
	var usage TokenUsage
	var usageAvail bool
	accUsage := func(u *gateway.Usage) {
		if tu, ok := UsageFromGateway(u); ok {
			usage = usage.Add(tu)
			usageAvail = true
		}
	}

	content, u1, err := requestPlanContentUsage(ctx, gw, cfg, messages)
	accUsage(u1)
	if err != nil {
		return nil, err
	}
	if d, perr := parseNextStep(content); perr == nil {
		d.Usage, d.UsageAvailable = usage, usageAvail
		return d, nil
	}

	// Corrective retry: echo the bad reply, demand JSON.
	corrective := make([]gateway.ChatMessage, 0, len(messages)+2)
	corrective = append(corrective, messages...)
	corrective = append(corrective,
		gateway.ChatMessage{Role: "assistant", Content: content},
		gateway.ChatMessage{Role: "user", Content: nextStepCorrection},
	)
	content2, u2, err2 := requestPlanContentUsage(ctx, gw, cfg, corrective)
	accUsage(u2)
	if err2 != nil {
		return nil, fmt.Errorf("next-step corrective retry: %w", err2)
	}
	d, perr2 := parseNextStep(content2)
	if perr2 != nil {
		return nil, fmt.Errorf("parse next-step JSON (after corrective retry): %w", perr2)
	}
	d.Usage, d.UsageAvailable = usage, usageAvail
	return d, nil
}

// parseNextStep extracts and decodes the single-action JSON object.
func parseNextStep(raw string) (*NextStepDecision, error) {
	jsonText := extractFirstJSONObject(raw)
	if jsonText == "" {
		return nil, fmt.Errorf("no JSON object in next-step reply")
	}
	var w nextStepWire
	if err := json.Unmarshal([]byte(jsonText), &w); err != nil {
		return nil, fmt.Errorf("unmarshal next-step: %w", err)
	}
	d := &NextStepDecision{Thought: strings.TrimSpace(w.Thought)}
	// Final wins when explicitly set. Then clarify (reactive pause). Then action.
	if w.Final {
		d.Final = true
		return d, nil
	}
	if w.Clarify != nil && strings.TrimSpace(w.Clarify.Question) != "" {
		d.Clarification = &ClarificationPayload{
			Question:         strings.TrimSpace(w.Clarify.Question),
			Kind:             strings.TrimSpace(w.Clarify.Kind),
			MissingFields:    w.Clarify.Missing,
			Options:          w.Clarify.Options,
			PendingTool:      strings.TrimSpace(w.Clarify.PendingTool),
			PartialArguments: w.Clarify.PartialArguments,
		}
		return d, nil
	}
	// No action provided → treat as final (mirrors pre-OGA-446 behavior).
	if w.Action == nil || strings.TrimSpace(w.Action.Tool) == "" {
		d.Final = true
		return d, nil
	}
	d.ToolName = strings.TrimSpace(w.Action.Tool)
	d.Arguments = w.Action.Arguments
	if d.Arguments == nil {
		d.Arguments = map[string]any{}
	}
	return d, nil
}

// renderToolPalette lists the available tools for the system prompt. When
// ToolSchemas are supplied, each tool renders with its description and a compact
// summary of its input arguments (names + types, required marked with *) so the
// model emits correct arguments rather than guessing (OGA-419 G1). Tools present
// only in schemas are appended to the palette; tools without a schema render as
// bare names. The union order is: names first (declaration order), then
// schema-only names.
func renderToolPalette(tools []string, schemas []ToolSchema) string {
	if len(tools) == 0 && len(schemas) == 0 {
		return ""
	}

	detail := make(map[string]ToolSchema, len(schemas))
	for _, s := range schemas {
		detail[s.Name] = s
	}

	ordered := make([]string, 0, len(tools)+len(schemas))
	seen := make(map[string]struct{}, len(tools)+len(schemas))
	for _, t := range tools {
		if _, dup := seen[t]; dup || t == "" {
			continue
		}
		seen[t] = struct{}{}
		ordered = append(ordered, t)
	}
	for _, s := range schemas {
		if _, dup := seen[s.Name]; dup || s.Name == "" {
			continue
		}
		seen[s.Name] = struct{}{}
		ordered = append(ordered, s.Name)
	}

	var b strings.Builder
	b.WriteString("\n\nAvailable tools:")
	for _, name := range ordered {
		b.WriteString("\n- ")
		b.WriteString(name)
		s, ok := detail[name]
		if !ok {
			continue
		}
		if s.Description != "" {
			b.WriteString(": ")
			b.WriteString(s.Description)
		}
		if args := summarizeSchema(s.InputSchema); args != "" {
			b.WriteString(" (args: ")
			b.WriteString(args)
			b.WriteString(")")
		}
	}
	return b.String()
}

// maxSchemaSummaryLen bounds the per-tool argument summary so a verbose schema
// can't blow the decision-prompt token budget.
const maxSchemaSummaryLen = 600

// summarizeSchema renders a compact, token-bounded view of a JSON Schema's
// input arguments: "name(type)*, other(string), ..." where * marks a required
// field. It reads the standard {type, properties, required} shape. When the
// schema can't be parsed into that shape it falls back to the compacted raw
// JSON, truncated to maxSchemaSummaryLen. Returns "" for an empty schema.
func summarizeSchema(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var doc struct {
		Properties map[string]struct {
			Type string `json:"type"`
			Enum []any  `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Properties) == 0 {
		return compactTruncate(raw, maxSchemaSummaryLen)
	}

	required := make(map[string]struct{}, len(doc.Required))
	for _, r := range doc.Required {
		required[r] = struct{}{}
	}

	// Stable order: required fields first (declaration order in `required`),
	// then the remaining properties sorted for determinism.
	names := make([]string, 0, len(doc.Properties))
	for n := range doc.Properties {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	write := func(name string) {
		p := doc.Properties[name]
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		b.WriteString(name)
		typ := p.Type
		if typ == "" && len(p.Enum) > 0 {
			typ = "enum"
		}
		if typ != "" {
			b.WriteString("(")
			b.WriteString(typ)
			// Render the allowed values for a constrained field so the model
			// picks a valid one instead of inventing a plausible-looking value
			// (e.g. mode "single" when the enum is range|multi) — OGA-431.
			if len(p.Enum) > 0 {
				b.WriteString("=")
				b.WriteString(summarizeEnum(p.Enum))
			}
			b.WriteString(")")
		}
		if _, req := required[name]; req {
			b.WriteString("*")
		}
	}
	for _, r := range doc.Required {
		if _, ok := doc.Properties[r]; ok {
			write(r)
		}
	}
	for _, n := range names {
		if _, req := required[n]; req {
			continue
		}
		write(n)
	}

	out := b.String()
	if len(out) > maxSchemaSummaryLen {
		out = out[:maxSchemaSummaryLen] + "…"
	}
	return out
}

// summarizeEnum renders a JSON Schema enum's allowed values as "a|b|c",
// bounded so a large enum can't blow the per-tool summary budget. Values are
// stringified with %v (covering string/number/bool). Caps at 12 values and
// 120 runes, appending "…" when truncated.
func summarizeEnum(values []any) string {
	const maxValues = 12
	const maxLen = 120
	parts := make([]string, 0, len(values))
	truncated := false
	for i, v := range values {
		if i >= maxValues {
			truncated = true
			break
		}
		parts = append(parts, fmt.Sprintf("%v", v))
	}
	out := strings.Join(parts, "|")
	if len(out) > maxLen {
		out = out[:maxLen]
		truncated = true
	}
	if truncated {
		out += "…"
	}
	return out
}

// compactTruncate compacts raw JSON (removing insignificant whitespace) and
// truncates it to maxLen runes.
func compactTruncate(raw json.RawMessage, maxLen int) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return ""
	}
	s := buf.String()
	if len(s) > maxLen {
		return s[:maxLen] + "…"
	}
	return s
}

// renderHints renders advisory grounding hints into the system prompt.
func renderHints(hints []GroundingHint) string {
	if len(hints) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nRecommended investigation steps (advisory — adapt to what you observe):")
	for i, h := range hints {
		fmt.Fprintf(&b, "\n%d. %s", i+1, h.Tool)
		if h.Rationale != "" {
			fmt.Fprintf(&b, " — %s", h.Rationale)
		}
		if h.StronglyAdvised {
			b.WriteString(" [strongly advised]")
		}
	}
	return b.String()
}

// renderNextStepUser builds the user message: seed facts + query + transcript.
func renderNextStepUser(req NextStepRequest) string {
	var b strings.Builder
	if strings.TrimSpace(req.SeedFacts) != "" {
		b.WriteString(req.SeedFacts)
		b.WriteString("\n\n")
	}
	b.WriteString("Question / task: ")
	b.WriteString(req.Query)
	if len(req.History) == 0 {
		b.WriteString("\n\nNo actions taken yet. Decide the first action.")
		return b.String()
	}
	b.WriteString("\n\nObservations so far:")
	for i, o := range req.History {
		fmt.Fprintf(&b, "\n[%d] tool=%s", i+1, o.ToolName)
		if len(o.Arguments) > 0 {
			if argsJSON, err := json.Marshal(o.Arguments); err == nil {
				fmt.Fprintf(&b, " args=%s", string(argsJSON))
			}
		}
		switch {
		case o.Skipped:
			fmt.Fprintf(&b, " -> SKIPPED: %s", o.Error)
		case !o.Success:
			fmt.Fprintf(&b, " -> ERROR: %s", o.Error)
		case strings.TrimSpace(o.Content) == "":
			b.WriteString(" -> EMPTY result")
		default:
			fmt.Fprintf(&b, " -> %s", o.Content)
		}
	}
	b.WriteString("\n\nDecide the next action, or finalize if you have enough.")
	return b.String()
}

// extractFirstJSONObject returns the substring from the first '{' to the last
// '}', stripping markdown fences. Returns "" when no object delimiters exist.
func extractFirstJSONObject(s string) string {
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
