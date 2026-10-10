package agent

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestProfile_ReactiveDelegation_Parses verifies the flat SDK profile schema
// (the shape the sidecar loads from /config/profile.yaml) unmarshals the
// reactive_delegation block, and that absence is the default opt-out (OGA-419).
func TestProfile_ReactiveDelegation_Parses(t *testing.T) {
	t.Parallel()

	t.Run("enabled", func(t *testing.T) {
		t.Parallel()
		const y = `
name: fm-operations-agent
port: ":8110"
reactive_delegation:
  knowledge_agent: true
`
		var p DomainAgentProfile
		if err := yaml.Unmarshal([]byte(y), &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if p.ReactiveDelegation == nil {
			t.Fatal("reactive_delegation block was dropped")
		}
		if !p.ReactiveDelegation.KnowledgeAgent {
			t.Error("knowledge_agent:true should parse as enabled")
		}
	})

	t.Run("absent is opt-out", func(t *testing.T) {
		t.Parallel()
		const y = `
name: fm-operations-agent
port: ":8110"
`
		var p DomainAgentProfile
		if err := yaml.Unmarshal([]byte(y), &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if p.ReactiveDelegation != nil {
			t.Errorf("absent reactive_delegation should remain nil, got %+v", p.ReactiveDelegation)
		}
	})

	t.Run("explicit false", func(t *testing.T) {
		t.Parallel()
		const y = `
name: fm-operations-agent
port: ":8110"
reactive_delegation:
  knowledge_agent: false
`
		var p DomainAgentProfile
		if err := yaml.Unmarshal([]byte(y), &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if p.ReactiveDelegation == nil || p.ReactiveDelegation.KnowledgeAgent {
			t.Error("knowledge_agent:false should parse as disabled")
		}
	})
}
