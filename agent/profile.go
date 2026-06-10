package agent

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// DomainAgentProfile contains the full configuration for a domain agent,
// loaded from the agent profile YAML file.
type DomainAgentProfile struct {
	// AgentID is the unique identifier for this agent instance.
	AgentID string `yaml:"agent_id"`

	// Name is the agent's display name.
	Name string `yaml:"name"`

	// Description is a human-readable description.
	Description string `yaml:"description"`

	// Version is the agent version.
	Version string `yaml:"version"`

	// Port is the HTTP port the agent listens on.
	Port string `yaml:"port"`

	// Category classifies the agent (e.g., "platform_addon", "customer_extension").
	Category string `yaml:"category"`

	// Domain is the vertical domain (e.g., "built-environment").
	Domain string `yaml:"domain"`

	// Skills lists the agent's capabilities.
	Skills []SkillDef `yaml:"skills"`

	// ProactiveReasoning configures proactive behavior.
	ProactiveReasoning *ProactiveConfig `yaml:"proactive_reasoning,omitempty"`

	// Capabilities lists named capability groups with their tools.
	Capabilities []CapabilityDef `yaml:"capabilities,omitempty"`

	// PBACBoundary defines access control limits.
	PBACBoundary *PBACBoundary `yaml:"pbac_boundary,omitempty"`

	// EventSubscriptions lists event topics this agent subscribes to.
	EventSubscriptions []EventSubscription `yaml:"event_subscriptions,omitempty"`
}

// SkillDef defines a skill in the agent profile.
type SkillDef struct {
	ID          string   `yaml:"id"`
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Tags        []string `yaml:"tags,omitempty"`
}

// ProactiveConfig configures proactive reasoning behavior.
type ProactiveConfig struct {
	SystemPrompt         string   `yaml:"system_prompt"`
	ToolCategories       []string `yaml:"tool_categories,omitempty"`
	ContextGatherTimeout string   `yaml:"context_gather_timeout,omitempty"`
	ReasoningTimeout     string   `yaml:"reasoning_timeout,omitempty"`

	// GroundingStrategy is the optional declarative tool-call plan.
	// When non-empty, DefaultRuntime.HandleStream uses GroundingStrategyPlanner
	// (deterministic, no LLM planning call). When empty, falls back to
	// LLMToolPlanner (dynamic per-request planning). See OGA-303.
	GroundingStrategy []GroundingStep `yaml:"grounding_strategy,omitempty"`

	// Actions is the declarative catalog of proactive actions this agent may
	// propose. Each action is the contract between the reasoning LLM and the
	// platform's execution + persistence layer. The reasoning LLM selects one
	// action from the candidate catalog (or declines) — the catalog is offered
	// as a discriminated decision schema, never a rule-based gate. See
	// proactive-action-handling design "Action Schema". (OGA-317)
	Actions []ActionDef `yaml:"actions,omitempty"`

	// Routing is the primary delivery target for proposals this agent submits.
	// REQUIRED when Actions is non-empty (validated at load → OGA-DKIT-VAL-1040)
	// so a misconfigured kit fails at install rather than at runtime. The
	// proactive handler packs it into ActionProposal.Routing for every proposal.
	Routing *RoutingDef `yaml:"routing,omitempty"`

	// EscalationPolicy declares where a proposal escalates when no operator
	// responds within Timeout, plus the notification hold window. Optional —
	// when absent, proposals carry no escalation routing and rely on platform
	// defaults. (OGA-317)
	EscalationPolicy *EscalationPolicyDef `yaml:"escalation_policy,omitempty"`
}

// GroundingStep is one step in a kit-declared grounding strategy. Each step
// runs a specific MCP tool with named-placeholder substitution from prior step
// results. Conditional execution (When) and required-step semantics (Required)
// preserve the kit author's declarative intent.
type GroundingStep struct {
	// Name is the human-readable identifier and placeholder key.
	Name string `yaml:"name"`

	// Tool is the MCP tool to invoke (e.g., "kg_search").
	Tool string `yaml:"tool"`

	// Arguments is the parameter map passed to the tool. Values may contain
	// {placeholder} tokens resolved from prior step results.
	Arguments map[string]any `yaml:"arguments,omitempty"`

	// Condition is a CEL expression evaluated at runtime. When false, the
	// step is skipped (a tool_call event with Skipped:true is emitted).
	// "true" / empty / missing → always run. "false" → always skip.
	// Other CEL expressions → not yet evaluated (treated as skip with a
	// "CEL evaluation not yet implemented" reason). Full CEL integration
	// is tracked as a follow-up.
	Condition string `yaml:"condition,omitempty"`

	// Required marks this step as fail-fast: a tool error stops the pipeline
	// with task/status{failed}. Non-required steps log and continue.
	Required bool `yaml:"required,omitempty"`

	// MaxResults caps the number of results returned (for tools that produce
	// JSON arrays). 0 = no cap.
	MaxResults int `yaml:"max_results,omitempty"`

	// DependsOn references a prior step by name. Resolved to step index
	// at planner time.
	DependsOn string `yaml:"depends_on,omitempty"`
}

// CapabilityDef defines a named capability with its tools.
type CapabilityDef struct {
	Name  string   `yaml:"name"`
	Tools []string `yaml:"tools"`
}

// PBACBoundary defines the access control boundary for the agent.
type PBACBoundary struct {
	AllowedEntityTypes []string `yaml:"allowed_entity_types,omitempty"`
	AllowedOperations  []string `yaml:"allowed_operations,omitempty"`
	DeniedOperations   []string `yaml:"denied_operations,omitempty"`
}

// EventSubscription defines an event topic subscription.
type EventSubscription struct {
	Topic  string `yaml:"topic"`
	Action string `yaml:"action"`
}

// LoadDomainAgentProfile reads and parses an agent profile from a YAML file.
func LoadDomainAgentProfile(path string) (*DomainAgentProfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read agent profile %s: %w", path, err)
	}

	var profile DomainAgentProfile
	if err := yaml.Unmarshal(data, &profile); err != nil {
		return nil, fmt.Errorf("parse agent profile %s: %w", path, err)
	}

	if profile.Name == "" {
		return nil, fmt.Errorf("agent profile %s: name is required", path)
	}
	if profile.Port == "" {
		profile.Port = "8200"
	}

	if err := validateActions(&profile); err != nil {
		return nil, fmt.Errorf("agent profile %s: %w", path, err)
	}

	return &profile, nil
}
