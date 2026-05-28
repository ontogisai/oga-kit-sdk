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

	return &profile, nil
}
