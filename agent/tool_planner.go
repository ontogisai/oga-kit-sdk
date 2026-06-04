// Package agent — tool planner.
//
// Domain agents implement a plan → execute → assemble loop so the LLM
// can ground its answers in actual tenant data via MCP tools (kg_search,
// kg_doc_content, kg_traversal, ...). The planner asks the LLM which
// tools to call, the runtime executes them via the Platform Access
// Gateway, and feeds results back to the LLM for final assembly.
//
// This mirrors the pattern used by the platform's Knowledge Agent
// (internal/agent/knowledge_agent.go: AgentgatewayToolPlanner).
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ontogisai/oga-kit-sdk/gateway"
)

// ToolStep is one step in an LLM-produced execution plan.
type ToolStep struct {
	// ToolName is the MCP tool to invoke (e.g., "kg_search").
	ToolName string `json:"tool_name"`

	// Arguments is the parameter map passed to the tool.
	Arguments map[string]any `json:"arguments"`

	// DependsOn is the 0-based index of a prior step whose output
	// this step needs, or -1 for no dependency.
	DependsOn int `json:"depends_on"`

	// Rationale is a short human-readable explanation.
	Rationale string `json:"rationale,omitempty"`
}

// ToolPlan is the parsed JSON plan returned by the LLM planner.
type ToolPlan struct {
	Steps []ToolStep `json:"steps"`
}

// ToolStepResult is the result of executing one ToolStep.
type ToolStepResult struct {
	ToolName      string          `json:"tool_name"`
	Success       bool            `json:"success"`
	Content       string          `json:"content,omitempty"`
	Result        json.RawMessage `json:"result,omitempty"`
	Error         string          `json:"error,omitempty"`
	ErrorCode     string          `json:"error_code,omitempty"`
	ErrorCategory string          `json:"error_category,omitempty"`
	ErrorDetails  map[string]any  `json:"error_details,omitempty"`
	LatencyMS     int64           `json:"latency_ms"`
}

// PlannerConfig configures the tool-calling loop.
type PlannerConfig struct {
	// MaxSteps caps the number of tool invocations per request to
	// prevent runaway loops. Defaults to 5.
	MaxSteps int

	// PlanTimeout caps the LLM planning call.
	PlanTimeout time.Duration

	// AssembleTimeout caps the final LLM assembly call.
	AssembleTimeout time.Duration

	// ToolTimeout caps each individual MCP tool call.
	ToolTimeout time.Duration

	// Model overrides the LLM model (empty = gateway default).
	Model string

	// MaxTokens caps LLM response tokens.
	MaxTokens int
}

// DefaultPlannerConfig returns conservative defaults.
func DefaultPlannerConfig() PlannerConfig {
	return PlannerConfig{
		MaxSteps:        5,
		PlanTimeout:     30 * time.Second,
		AssembleTimeout: 30 * time.Second,
		ToolTimeout:     30 * time.Second,
		MaxTokens:       2048,
	}
}

// gatewayClient is the subset of *gateway.PlatformGatewayClient the
// planner uses. Defined as an interface so tests can supply a fake.
type gatewayClient interface {
	ChatCompletion(ctx context.Context, req *gateway.ChatCompletionRequest) (*gateway.ChatCompletionResponse, error)
	CallTool(ctx context.Context, tool string, params any) (json.RawMessage, error)
}

// PlanAndExecute runs the full plan → execute → assemble loop.
//
//   - profile contributes the agent's system prompt and the union of
//     all tools declared across its capabilities.
//   - userText is the user's message.
//
// Returns the assembled natural-language response plus the per-step
// results (so callers can attach them as citations to the response
// metadata).
func PlanAndExecute(
	ctx context.Context,
	gw gatewayClient,
	profile *DomainAgentProfile,
	userText string,
	cfg PlannerConfig,
) (string, []ToolStepResult, error) {
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 5
	}
	if cfg.PlanTimeout <= 0 {
		cfg.PlanTimeout = 30 * time.Second
	}
	if cfg.AssembleTimeout <= 0 {
		cfg.AssembleTimeout = 30 * time.Second
	}
	if cfg.ToolTimeout <= 0 {
		cfg.ToolTimeout = 30 * time.Second
	}

	tools := uniqueTools(profile)
	if len(tools) == 0 {
		// No tools available — fall back to plain chat completion so
		// the agent still answers, just without grounding.
		slog.InfoContext(ctx, "agent: no tools declared in profile, falling back to plain LLM",
			"agent_id", profile.AgentID,
		)
		return plainAnswer(ctx, gw, profile, userText, cfg)
	}

	// Step 1: ask the LLM for an execution plan.
	planCtx, planCancel := context.WithTimeout(ctx, cfg.PlanTimeout)
	plan, err := requestPlan(planCtx, gw, profile, userText, tools, cfg)
	planCancel()
	if err != nil {
		// Planning failed (LLM unavailable, parse error). Fall back to
		// a plain answer so the agent stays useful in degraded mode.
		slog.WarnContext(ctx, "agent: planner failed, falling back to plain LLM",
			"agent_id", profile.AgentID,
			"error", err,
		)
		return plainAnswer(ctx, gw, profile, userText, cfg)
	}

	if len(plan.Steps) == 0 {
		// LLM decided no tools were needed — just answer directly.
		return plainAnswer(ctx, gw, profile, userText, cfg)
	}

	if len(plan.Steps) > cfg.MaxSteps {
		slog.WarnContext(ctx, "agent: plan exceeds MaxSteps, truncating",
			"agent_id", profile.AgentID,
			"plan_steps", len(plan.Steps),
			"max_steps", cfg.MaxSteps,
		)
		plan.Steps = plan.Steps[:cfg.MaxSteps]
	}

	// Step 2: execute each step in order via the gateway.
	results := make([]ToolStepResult, 0, len(plan.Steps))
	for i, step := range plan.Steps {
		stepCtx, stepCancel := context.WithTimeout(ctx, cfg.ToolTimeout)
		res := executeStep(stepCtx, gw, step)
		stepCancel()
		results = append(results, res)
		slog.InfoContext(ctx, "agent: tool step executed",
			"agent_id", profile.AgentID,
			"step_index", i,
			"tool", step.ToolName,
			"success", res.Success,
			"latency_ms", res.LatencyMS,
		)
	}

	// Step 3: ask the LLM to assemble the final answer from the
	// tool results. Even if all steps failed, we still call the
	// assembler so the user gets a coherent message.
	assembleCtx, assembleCancel := context.WithTimeout(ctx, cfg.AssembleTimeout)
	answer, err := assembleAnswer(assembleCtx, gw, profile, userText, results, cfg)
	assembleCancel()
	if err != nil {
		return "", results, fmt.Errorf("assemble answer: %w", err)
	}
	return answer, results, nil
}

// uniqueTools returns the union of all tools declared in the profile's
// capabilities, deduplicated and stable-ordered.
func uniqueTools(profile *DomainAgentProfile) []string {
	if profile == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, cap := range profile.Capabilities {
		for _, t := range cap.Tools {
			if t == "" {
				continue
			}
			if _, ok := seen[t]; ok {
				continue
			}
			seen[t] = struct{}{}
			out = append(out, t)
		}
	}
	return out
}

// requestPlan asks the LLM to produce a JSON ToolPlan for the user query.
func requestPlan(
	ctx context.Context,
	gw gatewayClient,
	profile *DomainAgentProfile,
	userText string,
	tools []string,
	cfg PlannerConfig,
) (*ToolPlan, error) {
	systemPrompt := planningSystemPrompt(profile, tools)

	req := &gateway.ChatCompletionRequest{
		Model:     cfg.Model,
		MaxTokens: cfg.MaxTokens,
		Messages: []gateway.ChatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userText},
		},
	}
	resp, err := gw.ChatCompletion(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("planning LLM call: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, errors.New("planning LLM call: no choices")
	}
	content := strings.TrimSpace(resp.Choices[0].Message.Content)
	if content == "" {
		return nil, errors.New("planning LLM call: empty content")
	}
	return parsePlan(content)
}

// parsePlan parses the LLM's JSON response into a ToolPlan, tolerating
// markdown code fences the LLM may wrap around the JSON.
func parsePlan(content string) (*ToolPlan, error) {
	// Strip markdown fences if present.
	content = strings.TrimSpace(content)
	if strings.HasPrefix(content, "```") {
		// Drop opening fence (possibly with language tag).
		if idx := strings.Index(content, "\n"); idx >= 0 {
			content = content[idx+1:]
		}
		// Drop closing fence.
		if idx := strings.LastIndex(content, "```"); idx >= 0 {
			content = content[:idx]
		}
		content = strings.TrimSpace(content)
	}

	var plan ToolPlan
	if err := json.Unmarshal([]byte(content), &plan); err != nil {
		return nil, fmt.Errorf("parse plan JSON: %w", err)
	}
	return &plan, nil
}

// executeStep invokes a single tool via the gateway and captures the result.
func executeStep(ctx context.Context, gw gatewayClient, step ToolStep) ToolStepResult {
	start := time.Now()
	res := ToolStepResult{ToolName: step.ToolName}
	raw, err := gw.CallTool(ctx, step.ToolName, step.Arguments)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Success = false
		// Check for structured ToolError — preserve the structured fields
		// so the assembler can provide actionable error messages.
		if toolErr, ok := err.(*gateway.ToolError); ok {
			res.Error = toolErr.Error()
			res.ErrorCode = toolErr.Code
			res.ErrorCategory = toolErr.Category
			res.ErrorDetails = toolErr.Details
		} else {
			res.Error = err.Error()
		}
		return res
	}
	res.Success = true
	res.Result = raw
	res.Content = string(raw)
	return res
}

// assembleAnswer asks the LLM to synthesize tool results into a final answer.
func assembleAnswer(
	ctx context.Context,
	gw gatewayClient,
	profile *DomainAgentProfile,
	userText string,
	results []ToolStepResult,
	cfg PlannerConfig,
) (string, error) {
	var resultCtx strings.Builder
	for i, r := range results {
		if r.Success {
			fmt.Fprintf(&resultCtx, "Tool %d: %s\nResult:\n%s\n\n", i+1, r.ToolName, r.Content)
		} else {
			fmt.Fprintf(&resultCtx, "Tool %d: %s\nError: %s\n", i+1, r.ToolName, r.Error)
			// Include structured error context so the LLM can give actionable advice.
			if r.ErrorCode != "" {
				fmt.Fprintf(&resultCtx, "  Error Code: %s\n", r.ErrorCode)
			}
			if r.ErrorCategory != "" {
				fmt.Fprintf(&resultCtx, "  Error Category: %s\n", r.ErrorCategory)
			}
			if len(r.ErrorDetails) > 0 {
				for k, v := range r.ErrorDetails {
					fmt.Fprintf(&resultCtx, "  %s: %v\n", k, v)
				}
			}
			resultCtx.WriteString("\n")
		}
	}

	systemPrompt := assemblySystemPrompt(profile)
	userPrompt := fmt.Sprintf("Original user question: %s\n\nTool results:\n%s", userText, resultCtx.String())

	req := &gateway.ChatCompletionRequest{
		Model:     cfg.Model,
		MaxTokens: cfg.MaxTokens,
		Messages: []gateway.ChatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
	}
	resp, err := gw.ChatCompletion(ctx, req)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("no choices in assembly response")
	}
	return strings.TrimSpace(resp.Choices[0].Message.Content), nil
}

// plainAnswer falls back to a single LLM call without tool grounding.
// Used when there are no tools, the planner fails, or the LLM decides
// no tools are needed.
func plainAnswer(
	ctx context.Context,
	gw gatewayClient,
	profile *DomainAgentProfile,
	userText string,
	cfg PlannerConfig,
) (string, []ToolStepResult, error) {
	systemPrompt := "You are a helpful domain agent."
	if profile != nil && profile.ProactiveReasoning != nil && profile.ProactiveReasoning.SystemPrompt != "" {
		systemPrompt = profile.ProactiveReasoning.SystemPrompt
	}

	req := &gateway.ChatCompletionRequest{
		Model:     cfg.Model,
		MaxTokens: cfg.MaxTokens,
		Messages: []gateway.ChatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userText},
		},
	}
	resp, err := gw.ChatCompletion(ctx, req)
	if err != nil {
		return "", nil, fmt.Errorf("plain answer: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", nil, errors.New("no choices in LLM response")
	}
	return strings.TrimSpace(resp.Choices[0].Message.Content), nil, nil
}

// planningSystemPrompt builds the system prompt instructing the LLM to
// produce a JSON tool-call plan.
func planningSystemPrompt(profile *DomainAgentProfile, tools []string) string {
	currentTime := time.Now().UTC().Format(time.RFC3339)

	var domainPrompt string
	if profile != nil && profile.ProactiveReasoning != nil && profile.ProactiveReasoning.SystemPrompt != "" {
		domainPrompt = profile.ProactiveReasoning.SystemPrompt + "\n\n"
	}

	var toolList strings.Builder
	for _, t := range tools {
		fmt.Fprintf(&toolList, "  - %s\n", t)
	}

	return fmt.Sprintf(`%sYou are a tool planning engine for a domain agent on the ONTOGIS AI Platform.
Your job is to analyze the user's question and produce an execution plan using the
MCP tools available to you. The platform will execute your plan and feed the
results back so you can produce the final answer.

Current date and time: %s

AVAILABLE TOOLS:
%s
RULES:
1. Return ONLY valid JSON — no markdown, no explanation, no code fences.
2. Select 0-5 tools that best answer the question.
3. If the question can be answered without tools (greeting, meta-question, opinion), return {"steps":[]}.
4. Order steps so dependent queries come after their prerequisites.
5. Use "depends_on" (0-based index) when a step needs output from a prior step. Use -1 for no dependency.
6. Only use tools from the AVAILABLE TOOLS list above.
7. Include a brief "rationale" for each step.
8. Arguments should match the tool's expected input schema.
9. When computing time ranges (e.g., "past 7 days"), use the Current date and time as reference.

OUTPUT FORMAT:
{"steps":[{"tool_name":"<name>","arguments":{...},"depends_on":-1,"rationale":"<why>"}]}
`, domainPrompt, currentTime, toolList.String())
}

// assemblySystemPrompt builds the prompt that synthesizes the final answer.
func assemblySystemPrompt(profile *DomainAgentProfile) string {
	var domainPrompt string
	if profile != nil && profile.ProactiveReasoning != nil && profile.ProactiveReasoning.SystemPrompt != "" {
		domainPrompt = profile.ProactiveReasoning.SystemPrompt + "\n\n"
	}

	return domainPrompt + `You are a domain agent on the ONTOGIS AI Platform. The platform has executed
the tool calls you planned and returned the results below. Your job is to:

1. Read the tool results and combine them into a coherent answer to the user's question.
2. Cite specific entities, documents, or measurements from the results when relevant —
   reference them by their IDs, names, or titles so the user can verify.
3. If a tool returned an error, acknowledge it gracefully and answer with what you have.
4. If the results are empty or insufficient, say so clearly rather than fabricating.
5. Match the tone and verbosity expected for this domain (concise, professional).

Do NOT mention the tools by name in the prose — just present the information.
Do NOT add disclaimers about being an AI or unable to access systems — you have just queried them.`
}
