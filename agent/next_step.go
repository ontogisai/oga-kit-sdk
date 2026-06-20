// Package agent — single-step ReAct decision call (OGA-419).
//
// RequestNextStep asks the LLM for ONE next action given the full observation
// transcript, replacing the plan-once RequestPlan on the interleaved path. The
// model returns either an action to take or a signal that it has enough
// evidence to answer. This is the core of the agentic reason -> act -> observe
// loop driven by streampipeline.Pipeline.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
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

// NextStepRequest is the input to a single ReAct decision call.
type NextStepRequest struct {
	// SystemPrompt is the agent persona/instructions (PlannerPersona.SystemPrompt).
	SystemPrompt string
	// Tools is the available tool palette — bounds what the model may pick.
	Tools []string
	// Query is the user/seed query.
	Query string
	// SeedFacts is resolved factual context (event facts / investigation context).
	SeedFacts string
	// Hints are advisory grounding hints (may be empty).
	Hints []GroundingHint
	// History is the observation transcript so far, in order.
	History []NextStepObservation
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
}

// nextStepWire is the JSON shape the model is asked to produce.
type nextStepWire struct {
	Thought string `json:"thought"`
	Final   bool   `json:"final"`
	Action  *struct {
		Tool      string         `json:"tool"`
		Arguments map[string]any `json:"arguments"`
	} `json:"action"`
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
	system := req.SystemPrompt + renderToolPalette(req.Tools) + renderHints(req.Hints) + nextStepContract
	user := renderNextStepUser(req)

	messages := []gateway.ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}

	content, err := requestPlanContent(ctx, gw, cfg, messages)
	if err != nil {
		return nil, err
	}
	if d, perr := parseNextStep(content); perr == nil {
		return d, nil
	}

	// Corrective retry: echo the bad reply, demand JSON.
	corrective := make([]gateway.ChatMessage, 0, len(messages)+2)
	corrective = append(corrective, messages...)
	corrective = append(corrective,
		gateway.ChatMessage{Role: "assistant", Content: content},
		gateway.ChatMessage{Role: "user", Content: nextStepCorrection},
	)
	content2, err2 := requestPlanContent(ctx, gw, cfg, corrective)
	if err2 != nil {
		return nil, fmt.Errorf("next-step corrective retry: %w", err2)
	}
	d, perr2 := parseNextStep(content2)
	if perr2 != nil {
		return nil, fmt.Errorf("parse next-step JSON (after corrective retry): %w", perr2)
	}
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
	// Final wins when set or when no action is provided.
	if w.Final || w.Action == nil || strings.TrimSpace(w.Action.Tool) == "" {
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

// renderToolPalette lists the available tools for the system prompt.
func renderToolPalette(tools []string) string {
	if len(tools) == 0 {
		return ""
	}
	return "\n\nAvailable tools: " + strings.Join(tools, ", ")
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
