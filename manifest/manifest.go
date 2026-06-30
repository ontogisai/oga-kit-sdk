package manifest

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/text/language"
	"gopkg.in/yaml.v3"
)

// KitManifest is the top-level structure of a domain kit manifest.yaml file.
type KitManifest struct {
	APIVersion string      `yaml:"api_version"`
	Kind       string      `yaml:"kind"`
	Metadata   KitMetadata `yaml:"metadata"`
	Spec       KitSpec     `yaml:"spec"`
}

// KitMetadata contains identifying information about the kit.
type KitMetadata struct {
	// Name is the kit identifier (e.g., "built-environment", "anycompany-maintenance").
	Name string `yaml:"name"`

	// Version is the semantic version of this kit release.
	Version string `yaml:"version"`

	// DisplayName is the human-readable name, keyed by full BCP-47 locale
	// tag (e.g., "en-US", "vi-VN"). Short-form keys ("en", "vi") are
	// rejected by Validate so kit authors can never silently disagree
	// with the platform's locale parsing. The convention is enforced
	// in OGA-51.
	DisplayName map[string]string `yaml:"display_name"`

	// Description is a detailed description, keyed by full BCP-47 locale
	// tag — same convention as DisplayName.
	Description map[string]string `yaml:"description"`

	// Authors lists the kit authors.
	Authors []Author `yaml:"authors,omitempty"`
}

// Author identifies a kit author.
type Author struct {
	Name  string `yaml:"name"`
	Email string `yaml:"email,omitempty"`
}

// KitSpec defines the kit's contents and configuration.
type KitSpec struct {
	// PlatformVersion is a semver constraint for platform compatibility.
	PlatformVersion string `yaml:"platform_version"`

	// Dependencies lists other kits that must be installed first.
	Dependencies []Dependency `yaml:"dependencies,omitempty"`

	// OntologyFiles lists paths to ontology YAML files (entity + relationship types).
	OntologyFiles []string `yaml:"ontology_files,omitempty"`

	// ExtensionFiles lists paths to regional extension YAML files.
	ExtensionFiles []string `yaml:"extension_files,omitempty"`

	// AgentProfiles lists paths to agent profile YAML files.
	AgentProfiles []string `yaml:"agent_profiles,omitempty"`

	// Tools lists paths to MCP tool definition YAML files.
	Tools []string `yaml:"tools,omitempty"`

	// PrivacyAnnotations lists paths to privacy annotation files.
	PrivacyAnnotations []string `yaml:"privacy_annotations,omitempty"`

	// SampleData lists paths to sample data files.
	SampleData []string `yaml:"sample_data,omitempty"`

	// SourceConnectors declares continuous-ingress Source Connectors the
	// platform deploys as sidecars at install time (continuous-ingress-connectors
	// spec). Replaces the former ingestion_templates field. Each connector
	// serves one or more (external_system, source_type) bindings; the platform
	// owns persistence, resolution, and validation of the records the connector
	// emits via the transfer contract.
	SourceConnectors []SourceConnectorSpec `yaml:"source_connectors,omitempty"`

	// GroundingDocuments lists grounding document definitions.
	GroundingDocuments []GroundingDocument `yaml:"grounding_documents,omitempty"`

	// Translations lists paths to i18n translation bundle files.
	Translations []string `yaml:"translations,omitempty"`

	// PromptFragments lists per-tenant LLM prompt fragments that augment
	// platform built-in agents (Frontier, Knowledge). Each entry MUST specify
	// both the target agent and the file path. The platform composes the
	// agent's system prompt from a neutral base + these fragments + dynamic
	// runtime context — see oga-platform spec
	// platform-agent-prompt-composition for the full design.
	PromptFragments []PromptFragmentEntry `yaml:"prompt_fragments,omitempty"`

	// Extensibility defines namespace policies for extension kits.
	Extensibility *Extensibility `yaml:"extensibility,omitempty"`

	// Workflows lists workflow configurations.
	Workflows []WorkflowConfig `yaml:"workflows,omitempty"`

	// Policies lists kit-scoped PBAC policies the platform installer
	// registers at install time (OGA-277). Each entry produces one Policy
	// record under the tenant's namespace with the deterministic ID
	//
	//   kit-{kit_id}-{tenant_id}-{id_suffix}
	//
	// Re-installs upsert via the same ID; uninstall archives (does NOT hard-
	// delete) so audit trails survive. The shape mirrors the platform's
	// internal/domainkit.KitPolicySpec exactly so YAML authored against
	// either type round-trips through the other without translation.
	//
	// See the platform's pbac-enhancements design doc (Component K3 —
	// Domain Kit Manifest Schema) for authoring guidance and the full set
	// of CEL activation variables expressions can reference.
	Policies []KitPolicySpec `yaml:"policies,omitempty"`

	// Monitors lists kit-declared time-series anomaly monitors (OGA-319). The
	// anomaly-monitor service runs detection per (tenant, source_id, metric)
	// for the declared (entity_type, metric) pairs and materializes
	// EntityAnomalyEvent triggers. The shape mirrors the platform's
	// internal/anomalymonitor.RawMonitorConfig exactly so YAML authored against
	// either type round-trips without translation.
	Monitors []MonitorSpec `yaml:"monitors,omitempty"`
}

// PromptFragmentEntry declares a prompt fragment file targeting a specific
// platform built-in agent. Both fields are required.
//
// Recognized targets:
//   - "frontier"  — augments the Frontier intent-classification + system prompt
//   - "knowledge" — augments the Knowledge tool planner + assembly prompt
//   - "analytics" — reserved for kit-supplied analytics agents (the platform
//     ships no built-in for this target)
type PromptFragmentEntry struct {
	// Target identifies the built-in agent the fragment augments.
	Target string `yaml:"target"`

	// File is the path (relative to the kit root) of the fragment text file.
	File string `yaml:"file"`
}

// PromptFragmentTarget enumerates the recognized values for
// PromptFragmentEntry.Target.
type PromptFragmentTarget = string

// Recognized prompt fragment targets.
const (
	PromptFragmentTargetFrontier  PromptFragmentTarget = "frontier"
	PromptFragmentTargetKnowledge PromptFragmentTarget = "knowledge"
	PromptFragmentTargetAnalytics PromptFragmentTarget = "analytics"
)

// IsValidPromptFragmentTarget reports whether t is one of the recognized
// prompt fragment target values.
func IsValidPromptFragmentTarget(t string) bool {
	switch t {
	case PromptFragmentTargetFrontier,
		PromptFragmentTargetKnowledge,
		PromptFragmentTargetAnalytics:
		return true
	default:
		return false
	}
}

// Dependency declares a required kit dependency.
type Dependency struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
}

// GroundingDocument defines a document to be ingested for agent grounding.
type GroundingDocument struct {
	Path     string                 `yaml:"path"`
	Metadata map[string]interface{} `yaml:"metadata,omitempty"`
}

// Extensibility defines namespace policies for extension kits.
type Extensibility struct {
	AllowCustomEntityTypes   bool      `yaml:"allow_custom_entity_types"`
	AllowCustomRelationships bool      `yaml:"allow_custom_relationships"`
	AllowCustomProperties    bool      `yaml:"allow_custom_properties"`
	ExtensionNamespacePrefix string    `yaml:"extension_namespace_prefix"`
	ProtectedNamespaces      []string  `yaml:"protected_namespaces,omitempty"`
	NamespacePolicy          *NSPolicy `yaml:"namespace_policy,omitempty"`
}

// NSPolicy defines namespace access levels.
type NSPolicy struct {
	Locked     []string `yaml:"locked,omitempty"`
	Extendable []string `yaml:"extendable,omitempty"`
	Open       []string `yaml:"open,omitempty"`
}

// WorkflowConfig defines a workflow configuration for the kit.
type WorkflowConfig struct {
	Name   string         `yaml:"name"`
	Type   string         `yaml:"type"`
	Config map[string]any `yaml:"config,omitempty"`
}

// SourceConnectorSpec declares one continuous-ingress Source Connector the
// platform deploys as a sidecar at install time. The platform stamps tenancy
// from the connector's authenticated workload identity; the connector only
// adapts the external system(s) and emits records via the transfer contract.
type SourceConnectorSpec struct {
	// Name is the connector instance name (unique within the kit; forms the
	// sidecar registry name {tenant}.{name}).
	Name string `yaml:"name"`

	// Image is the container image reference (with digest) for the connector
	// sidecar.
	Image string `yaml:"image"`

	// Bindings are the (external_system, source_type) feeds this connector
	// serves. At least one is required.
	Bindings []SourceBindingSpec `yaml:"bindings"`

	// CredentialRefs lists SecretStore secret names the platform injects for
	// the connector to authenticate to its external systems (one or more).
	CredentialRefs []string `yaml:"credential_refs,omitempty"`

	// Container holds optional deployment overrides (image already lives at
	// the top level; this carries port, resources, and env). The most common
	// use is env: per-tenant, non-baked configuration such as an external
	// system's base URL delivered via a SecretStore reference, e.g.
	//   container:
	//     env:
	//       WO_MGMT_URL: "secret://wo-mgmt-url"
	// The platform resolves a "secret://<name>" env value to the tenant-scoped
	// SecretStore secret at connector deploy time. Mirrors the platform's
	// domainkit.SidecarContainerSpec field-for-field (continuous-ingress-
	// connectors, OGA-437).
	Container SidecarContainerSpec `yaml:"container,omitempty"`
}

// SidecarContainerSpec holds optional deployment overrides for a sidecar the
// platform deploys from a kit manifest declaration. It mirrors the platform's
// internal domainkit.SidecarContainerSpec field-for-field so a kit author gets
// the same parse result locally as at install time.
type SidecarContainerSpec struct {
	// Image overrides the image reference (specs that declare image at the
	// top level leave this empty).
	Image string `yaml:"image,omitempty"`

	// Port overrides the container port; zero lets the platform allocate from
	// the sidecar role's range.
	Port int `yaml:"port,omitempty"`

	// Resources is the canonical home for memory + CPU limits. When absent the
	// flat MemoryMB / CPUMilli fields are used as fallbacks.
	Resources SidecarResourceSpec `yaml:"resources,omitempty"`

	// MemoryMB / CPUMilli are legacy flat aliases for the Resources block.
	MemoryMB int `yaml:"memory_mb,omitempty"`
	CPUMilli int `yaml:"cpu_milli,omitempty"`

	// Env injects additional environment variables into the container. A value
	// of the form "secret://<name>" is resolved by the platform to the
	// tenant-scoped SecretStore secret <name> at deploy time; any other value
	// is injected verbatim.
	Env map[string]string `yaml:"env,omitempty"`
}

// SidecarResourceSpec is the nested resources: block of a SidecarContainerSpec.
type SidecarResourceSpec struct {
	MemoryMB int `yaml:"memory_mb,omitempty"`
	CPUMilli int `yaml:"cpu_milli,omitempty"`
}

// SourceBindingSpec is one (external_system, source_type) feed of a connector.
type SourceBindingSpec struct {
	// ID is the connector-unique binding identifier (stable; used in the
	// internal webhook path and the platform IngressToken mapping).
	ID string `yaml:"id"`

	// ExternalSystem is the system of record (e.g. "contract_wo_mgmt").
	ExternalSystem string `yaml:"external_system"`

	// SourceType selects the record class (e.g. "wo_status_feed").
	SourceType string `yaml:"source_type"`

	// Modes declares ingress modes for this binding: "poll" and/or "webhook".
	// Empty defaults to poll.
	Modes []string `yaml:"modes,omitempty"`

	// TimeseriesMapping is the per-binding template for timeseries bindings:
	// tag→metric/unit conventions and how entity_id is derived. Concrete
	// source_id→entity_id bindings are per-tenant config, not kit-baked.
	TimeseriesMapping *TimeseriesMappingSpec `yaml:"timeseries_mapping,omitempty"`

	// RetentionPolicy names a timeseries retention/downsampling policy for the
	// binding's points (optional).
	RetentionPolicy string `yaml:"retention_policy,omitempty"`
}

// TimeseriesMappingSpec is the kit-authored template for mapping a timeseries
// source's tags onto the universal TimeSeriesPoint shape.
type TimeseriesMappingSpec struct {
	// EntityIDFrom names how to derive the bound entity_id (e.g. "source_id"
	// or a tag name). Concrete bindings are resolved/overridden per tenant.
	EntityIDFrom string `yaml:"entity_id_from,omitempty"`

	// MetricTag / UnitTag name the source tags carrying the metric and unit.
	MetricTag string `yaml:"metric_tag,omitempty"`
	UnitTag   string `yaml:"unit_tag,omitempty"`
}

// Recognized source-connector binding ingress modes.
const (
	SourceModePoll    = "poll"
	SourceModeWebhook = "webhook"
)

func isValidSourceMode(m string) bool {
	return m == SourceModePoll || m == SourceModeWebhook
}

// KitPolicySpec declares one PBAC policy that the platform installer should
// register at install time (OGA-277). The shape mirrors the platform-side
// internal/domainkit.KitPolicySpec exactly, plus the same Level field, so a
// single YAML body works for both routing-level and data-level policies.
//
// Required fields: IDSuffix, Level, Name, Expression.
//
// All Target* fields are optional and follow the matcher semantics
// implemented by the platform's PBAC engine — empty list = match all.
//
// At install time the deterministic policy ID is computed as
//
//	kit-{kit_id}-{tenant_id}-{id_suffix}
//
// (see the platform's domainkit.KitPolicyID). The "kit-" prefix lets
// operator tooling distinguish kit-installed policies from operator-authored
// ones (no prefix) and platform tenant defaults (which use "tenant-default-").
type KitPolicySpec struct {
	// IDSuffix is the kebab-case unique identifier within this kit's
	// policy set. Combined with the kit ID and tenant ID at install time
	// to produce the policy's deterministic ID. Must match the regex
	// ^[a-z][a-z0-9-]{1,40}[a-z0-9]$.
	IDSuffix string `yaml:"id_suffix"`

	// Level controls which evaluation tier the policy participates in:
	//   - "routing" — agent invocation gate (Level 1). The AccessRequest's
	//     Resource.EntityType is set to "agent" and EntityID to the target
	//     agent_id; resource_classification / resource_h3_* are empty.
	//   - "data" — entity access gate (Level 2). The AccessRequest carries
	//     real entity properties (classification, h3_cells, valid_from/to)
	//     populated by the MCP handler's loadResourceContext.
	Level string `yaml:"level"`

	// Name is a short human-readable label shown in operator tooling.
	Name string `yaml:"name"`

	// Description is a longer rationale explaining what the policy gates
	// and why. Optional but strongly recommended for operator clarity.
	Description string `yaml:"description"`

	// Priority orders policy evaluation: lower values evaluate first. The
	// first applicable DENY wins; otherwise the first applicable PERMIT
	// decides. Tenant-default policies typically use 100..300; kit
	// policies usually slot below the tenant defaults (e.g., 30..200).
	Priority int `yaml:"priority"`

	// Expression is the CEL boolean expression evaluated against the
	// request. It must evaluate to TRUE for access to be granted (kit
	// policies are permit-by-default). Available activation variables —
	// see the platform's pbac-enhancements design doc for the full list:
	//   principal_id           string
	//   principal_roles        list<string>
	//   principal_clearance    string
	//   principal_h3_cells     list<string>
	//   resource_entity_type   string
	//   resource_entity_id     string
	//   resource_classification string
	//   resource_h3_cell       string
	//   resource_h3_cells      list<string>
	//   resource_valid_from    timestamp
	//   resource_valid_to      timestamp
	//   action                 string  ("read"|"write"|"delete"|"traverse"|"invoke")
	Expression string `yaml:"expression"`

	// TargetRoles, TargetAgentIDs, TargetEntityTypes, TargetActions are
	// optional scope filters. An empty list matches all values; otherwise
	// the policy applies only when the request's corresponding field
	// intersects the list. Compile-time syntax + type-check of Expression
	// is left for the engine's CreatePolicy call at install time so the
	// SDK manifest validator does not need to import cel-go.
	TargetRoles       []string `yaml:"target_roles,omitempty"`
	TargetAgentIDs    []string `yaml:"target_agent_ids,omitempty"`
	TargetEntityTypes []string `yaml:"target_entity_types,omitempty"`
	TargetActions     []string `yaml:"target_actions,omitempty"`
}

// PolicyLevelRouting and PolicyLevelData are the recognized values of
// KitPolicySpec.Level. Mirrors internal/domainkit.PolicyLevelRouting /
// PolicyLevelData on the platform side.
const (
	PolicyLevelRouting = "routing"
	PolicyLevelData    = "data"
)

// kitPolicyIDSuffixPattern is the canonical id_suffix shape: kebab-case,
// 3-42 characters, lowercase letters/digits/hyphens, must start with a
// letter and end with a letter or digit. Mirrors the platform's
// internal/domainkit.kitPolicyIDSuffixPattern exactly.
const kitPolicyIDSuffixPattern = `^[a-z][a-z0-9-]{1,40}[a-z0-9]$`

var kitPolicyIDSuffixRegex = regexp.MustCompile(kitPolicyIDSuffixPattern)

// IsValidPolicyLevel reports whether s is one of the recognized policy
// level values (routing | data).
func IsValidPolicyLevel(s string) bool {
	return s == PolicyLevelRouting || s == PolicyLevelData
}

// Parse reads and parses a manifest from the given reader.
func Parse(r io.Reader) (*KitManifest, error) {
	var m KitManifest
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

// ParseFile reads and parses a manifest from the given file path.
func ParseFile(path string) (*KitManifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open manifest %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return Parse(f)
}

// Validate checks the manifest for structural correctness.
func Validate(m *KitManifest) error {
	if m.APIVersion == "" {
		return fmt.Errorf("api_version is required")
	}
	if m.Kind == "" {
		return fmt.Errorf("kind is required")
	}
	if m.Kind != "DomainKitManifest" {
		return fmt.Errorf("kind must be DomainKitManifest, got %q", m.Kind)
	}
	if m.Metadata.Name == "" {
		return fmt.Errorf("metadata.name is required")
	}
	if m.Metadata.Version == "" {
		return fmt.Errorf("metadata.version is required")
	}
	if err := validateLocaleKeys("metadata.display_name", m.Metadata.DisplayName); err != nil {
		return err
	}
	if err := validateLocaleKeys("metadata.description", m.Metadata.Description); err != nil {
		return err
	}
	if m.Spec.PlatformVersion == "" {
		return fmt.Errorf("spec.platform_version is required")
	}
	for i, e := range m.Spec.PromptFragments {
		if e.Target == "" {
			return fmt.Errorf("spec.prompt_fragments[%d]: target is required", i)
		}
		if !IsValidPromptFragmentTarget(e.Target) {
			return fmt.Errorf(
				"spec.prompt_fragments[%d]: target %q is not recognized "+
					"(valid: frontier, knowledge, analytics)",
				i, e.Target,
			)
		}
		if e.File == "" {
			return fmt.Errorf("spec.prompt_fragments[%d]: file is required", i)
		}
	}
	if err := validateKitPolicies(m.Spec.Policies); err != nil {
		return err
	}
	if err := validateSourceConnectors(m.Spec.SourceConnectors); err != nil {
		return err
	}
	if err := validateMonitors(m.Spec.Monitors); err != nil {
		return err
	}
	return nil
}

// ValidateLocaleKeys reports whether every key in m is a valid full
// BCP-47 language tag (e.g., "en-US", "vi-VN", "zh-CN"). Short-form
// language-only tags ("en", "vi") are rejected — kit manifests must
// use the full form so the platform's locale parser cannot silently
// disagree on which tag the kit means.
//
// The fieldName argument prefixes any error returned so the kit
// author can find the offending map quickly. An empty or nil map is
// always valid.
//
// This is the kit-facing entry point for validating any locale-keyed
// map a kit author maintains (display names on entity types, property
// descriptions, etc.). The transfer package re-exports the same
// helper as transfer.ValidateLocaleKeys so kit code that does not
// import manifest still has access.
func ValidateLocaleKeys(fieldName string, m map[string]string) error {
	return validateLocaleKeys(fieldName, m)
}

// validateLocaleKeys is the package-internal implementation behind
// the public ValidateLocaleKeys. Iteration order is sorted so error
// messages are stable across map random ordering.
func validateLocaleKeys(fieldName string, m map[string]string) error {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "" {
			return fmt.Errorf(
				"%s: locale key is empty (use full BCP-47 like \"en-US\", \"vi-VN\")",
				fieldName,
			)
		}
		if _, err := language.Parse(k); err != nil {
			return fmt.Errorf(
				"%s: locale key %q is not a valid BCP-47 tag: %w",
				fieldName, k, err,
			)
		}
		// Reject short-form language-only tags. golang.org/x/text/language
		// will happily *infer* a likely region for "en" / "vi" with Low
		// confidence — that's useful at runtime but unsafe for a
		// declarative manifest. Kit authors must spell out the region
		// (en-US vs en-GB; vi-VN vs zh-Hant-VN; etc.) so the kit's
		// intent is unambiguous when the supported-locale list grows.
		// Reject anything missing a "-" since every full BCP-47 tag has
		// at least one subtag separator (language-region or
		// language-script-region). Anything more nuanced is overkill —
		// the platform also enforces the same constraint at lookup time.
		if !strings.Contains(k, "-") {
			return fmt.Errorf(
				"%s: locale key %q must be a full BCP-47 tag with a region "+
					"(e.g., %q-US, %q-GB) — short-form language-only tags are rejected",
				fieldName, k, k, k,
			)
		}
	}
	return nil
}

// validateKitPolicies enforces the per-entry rules of spec.policies (OGA-277):
// id_suffix matches the canonical regex, level is "routing" or "data", name
// and expression are non-empty, and id_suffix is unique within the kit.
//
// The CEL expression's syntax + type-check is left for the platform
// installer's CreatePolicy call at install time so we don't pull cel-go
// into the SDK's manifest-only path.
//
// Mirrors the platform's internal/domainkit.validateKitPolicies exactly so
// kit authors get the same field-level error messages whether they validate
// locally with this SDK or fail at install time on a tenant's platform.
func validateKitPolicies(entries []KitPolicySpec) error {
	if len(entries) == 0 {
		return nil
	}
	seen := make(map[string]int, len(entries))
	for i := range entries {
		e := &entries[i]
		if e.IDSuffix == "" {
			return fmt.Errorf("spec.policies[%d]: id_suffix is required", i)
		}
		if !kitPolicyIDSuffixRegex.MatchString(e.IDSuffix) {
			return fmt.Errorf(
				"spec.policies[%d]: id_suffix = %q does not match %s",
				i, e.IDSuffix, kitPolicyIDSuffixPattern,
			)
		}
		if prev, ok := seen[e.IDSuffix]; ok {
			return fmt.Errorf(
				"spec.policies[%d]: id_suffix = %q duplicates spec.policies[%d]",
				i, e.IDSuffix, prev,
			)
		}
		seen[e.IDSuffix] = i

		switch e.Level {
		case PolicyLevelRouting, PolicyLevelData:
		case "":
			return fmt.Errorf("spec.policies[%d]: level is required", i)
		default:
			return fmt.Errorf(
				"spec.policies[%d]: level = %q is invalid; must be %q or %q",
				i, e.Level, PolicyLevelRouting, PolicyLevelData,
			)
		}

		if e.Name == "" {
			return fmt.Errorf("spec.policies[%d]: name is required", i)
		}
		if e.Expression == "" {
			return fmt.Errorf("spec.policies[%d]: expression is required", i)
		}
	}
	return nil
}

// Monitor detection methods (OGA-319). MVP supports zscore + threshold;
// forecast-based detection is deferred.
const (
	MonitorMethodZScore    = "zscore"
	MonitorMethodThreshold = "threshold"
)

// MonitorSpec is a kit-declared time-series anomaly monitor (spec.monitors[]).
// It mirrors the platform's internal/anomalymonitor.RawMonitorConfig field-for-
// field (and YAML-tag-for-tag) so a manifest authored against this SDK type
// validates identically at platform install time.
//
// entity_type + metric select which entities' series to watch; method selects
// the detector. The damping/cadence fields are optional — the platform fills
// any unset field from configs/anomaly-monitor.yaml defaults.
type MonitorSpec struct {
	EntityType           string   `yaml:"entity_type"`
	Metric               string   `yaml:"metric"`
	Method               string   `yaml:"method"`
	Sensitivity          float64  `yaml:"sensitivity,omitempty"`
	Upper                *float64 `yaml:"upper,omitempty"`
	Lower                *float64 `yaml:"lower,omitempty"`
	MinDuration          string   `yaml:"min_duration,omitempty"`
	MinConsecutive       int      `yaml:"min_consecutive,omitempty"`
	ClearAfter           string   `yaml:"clear_after,omitempty"`
	Cooldown             string   `yaml:"cooldown,omitempty"`
	Cadence              string   `yaml:"cadence,omitempty"`
	ReescalateOnSeverity bool     `yaml:"reescalate_on_severity,omitempty"`
	ReescalateCooldown   string   `yaml:"reescalate_cooldown,omitempty"`
}

// validateSourceConnectors checks each kit-declared Source Connector
// (spec.source_connectors[]): name + image present, at least one binding, each
// binding has id + external_system + source_type with valid modes, and binding
// IDs are unique within the connector. Connector names must be unique within
// the kit. Mirrors the field-level rules the platform installer applies so kit
// authors get the same errors locally as at install time.
func validateSourceConnectors(conns []SourceConnectorSpec) error {
	if len(conns) == 0 {
		return nil
	}
	seenConn := make(map[string]int, len(conns))
	for i := range conns {
		c := &conns[i]
		if c.Name == "" {
			return fmt.Errorf("spec.source_connectors[%d]: name is required", i)
		}
		if prev, ok := seenConn[c.Name]; ok {
			return fmt.Errorf(
				"spec.source_connectors[%d]: name = %q duplicates spec.source_connectors[%d]",
				i, c.Name, prev,
			)
		}
		seenConn[c.Name] = i
		if c.Image == "" {
			return fmt.Errorf("spec.source_connectors[%d]: image is required", i)
		}
		if len(c.Bindings) == 0 {
			return fmt.Errorf("spec.source_connectors[%d]: at least one binding is required", i)
		}
		seenBind := make(map[string]int, len(c.Bindings))
		for j := range c.Bindings {
			b := &c.Bindings[j]
			if b.ID == "" {
				return fmt.Errorf("spec.source_connectors[%d].bindings[%d]: id is required", i, j)
			}
			if prev, ok := seenBind[b.ID]; ok {
				return fmt.Errorf(
					"spec.source_connectors[%d].bindings[%d]: id = %q duplicates bindings[%d]",
					i, j, b.ID, prev,
				)
			}
			seenBind[b.ID] = j
			if b.ExternalSystem == "" {
				return fmt.Errorf("spec.source_connectors[%d].bindings[%d]: external_system is required", i, j)
			}
			if b.SourceType == "" {
				return fmt.Errorf("spec.source_connectors[%d].bindings[%d]: source_type is required", i, j)
			}
			for _, mode := range b.Modes {
				if !isValidSourceMode(mode) {
					return fmt.Errorf(
						"spec.source_connectors[%d].bindings[%d]: mode = %q is invalid; must be %q or %q",
						i, j, mode, SourceModePoll, SourceModeWebhook,
					)
				}
			}
		}
	}
	return nil
}

// validateMonitors checks each kit-declared anomaly monitor. Mirrors the
// platform's internal/domainkit.validateMonitors + anomalymonitor validation so
// kit authors get the same field-level errors locally as at install time.
func validateMonitors(monitors []MonitorSpec) error {
	for i := range monitors {
		m := &monitors[i]
		if m.EntityType == "" || m.Metric == "" {
			return fmt.Errorf("spec.monitors[%d]: entity_type and metric are required", i)
		}
		method := m.Method
		if method == "" {
			method = MonitorMethodZScore
		}
		if method != MonitorMethodZScore && method != MonitorMethodThreshold {
			return fmt.Errorf(
				"spec.monitors[%d]: method = %q is invalid; must be %q or %q",
				i, m.Method, MonitorMethodZScore, MonitorMethodThreshold,
			)
		}
		if method == MonitorMethodThreshold && m.Upper == nil && m.Lower == nil {
			return fmt.Errorf("spec.monitors[%d]: threshold monitor requires at least one of upper/lower", i)
		}
		for field, val := range map[string]string{
			"min_duration":        m.MinDuration,
			"clear_after":         m.ClearAfter,
			"cooldown":            m.Cooldown,
			"cadence":             m.Cadence,
			"reescalate_cooldown": m.ReescalateCooldown,
		} {
			if val == "" {
				continue
			}
			if _, err := time.ParseDuration(val); err != nil {
				return fmt.Errorf("spec.monitors[%d]: %s = %q is not a valid duration", i, field, val)
			}
		}
	}
	return nil
}
