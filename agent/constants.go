// Package agent — constants.
//
// All prompt templates, MCP tool names, and tool category lists live here so
// they can be updated in one place. Domain agents in this SDK and the
// platform's Knowledge Agent share the same prompt vocabulary so the LLM
// planner produces consistent plans regardless of which agent is invoked.
//
// When a kit author wants to extend a planner prompt for vertical-specific
// guidance, they write that guidance into the agent profile's
// `proactive_reasoning.system_prompt` (see ProactiveConfig). The SDK's
// PlanningSystemPrompt composes that domain prompt at the top of the
// PlannerPromptTemplate below.
package agent

// ─────────────────────────────────────────────────────────────────────────────
// MCP Tool Names — single source of truth.
//
// These constants mirror the tool names registered by the platform's MCP Tool
// Server (internal/mcptoolserver). When adding a new tool to the platform,
// add a constant here so kit authors can reference it in agent profiles
// without hardcoding strings.
// ─────────────────────────────────────────────────────────────────────────────
const (
	// CRUD (kg_crud category)
	ToolKGGetEntity          = "kg_get_entity"
	ToolKGCreateEntity       = "kg_create_entity"
	ToolKGUpdateEntity       = "kg_update_entity"
	ToolKGDeleteEntity       = "kg_delete_entity"
	ToolKGCreateRelationship = "kg_create_relationship"
	ToolKGDeleteRelationship = "kg_delete_relationship"

	// Query (kg_query category)
	ToolKGSearch             = "kg_search"
	ToolKGQueryEntities      = "kg_query_entities"
	ToolKGTraverse           = "kg_traverse"
	ToolKGQueryRelationships = "kg_query_relationships"
	ToolKGGeoTemporal        = "kg_geotemporal"
	ToolKGVector             = "kg_vector"
	ToolKGReason             = "kg_reason"

	// Timeseries (kg_timeseries category)
	ToolKGTSRead    = "kg_ts_read"
	ToolKGTSAnalyze = "kg_ts_analyze"

	// Schema (kg_schema category)
	ToolKGSearchEntityTypes = "kg_search_entity_types"
	ToolKGDescribeType      = "kg_describe_type"

	// Document (kg_document category)
	ToolKGDocContent = "kg_doc_content"
	ToolKGDocSearch  = "kg_doc_search"
	ToolKGDocUpload  = "kg_doc_upload"
	ToolKGDocDelete  = "kg_doc_delete"
	ToolKGDocStatus  = "kg_doc_status"
)

// ─────────────────────────────────────────────────────────────────────────────
// Tool Categories — must match CategoryKG* constants in
// internal/mcptoolserver/tool_def.go on the platform side.
// ─────────────────────────────────────────────────────────────────────────────
const (
	CategoryKGCRUD       = "kg_crud"
	CategoryKGQuery      = "kg_query"
	CategoryKGTimeseries = "kg_timeseries"
	CategoryKGSchema     = "kg_schema"
	CategoryKGDocument   = "kg_document"
)

// KnowledgeAgentToolCategories is the set of MCP tool categories the platform
// Knowledge Agent uses. Kit-supplied domain agents that want the same toolbox
// as the KA should reference this list in their proactive_reasoning.tool_categories.
var KnowledgeAgentToolCategories = []string{
	CategoryKGCRUD,
	CategoryKGQuery,
	CategoryKGTimeseries,
	CategoryKGSchema,
	CategoryKGDocument,
}

// StandardKnowledgeReadTools is the read-side tool set the platform Knowledge
// Agent plans against. Kit authors should include these tools in at least one
// capability so the LLM planner has the same toolbox the KA does.
//
// Write tools (kg_create_entity, kg_update_entity, etc.) are intentionally
// excluded — kit authors opt into them per-capability based on the agent's
// PBAC boundary.
var StandardKnowledgeReadTools = []string{
	ToolKGSearch,
	ToolKGQueryEntities,
	ToolKGGetEntity,
	ToolKGTraverse,
	ToolKGGeoTemporal,
	ToolKGVector,
	ToolKGReason,
	ToolKGTSRead,
	ToolKGTSAnalyze,
	ToolKGSearchEntityTypes,
	ToolKGDescribeType,
	ToolKGDocContent,
	ToolKGDocSearch,
}

// ─────────────────────────────────────────────────────────────────────────────
// Planner Prompt Template
//
// This is the same shape the platform's Knowledge Agent uses
// (internal/agent/knowledge_agent.go knowledgeAgentPlanningPrompt). Kit
// agents use it identically so the LLM planner produces consistent plans
// across the KA and kit-supplied agents.
//
// Placeholders (in fmt.Sprintf order):
//
//	%s (1st) — Domain prompt from profile.ProactiveReasoning.SystemPrompt
//	           (kit-author content). Empty string if not provided.
//	%s (2nd) — Current RFC3339 timestamp.
//	%s (3rd) — Available MCP tool descriptions (one per line).
//	%s (4th) — Current 4-digit year (for time-range rules).
//	%s (5th) — Example RFC3339 timestamp (same as 2nd, used as example).
//
// The template is intentionally vertical-NEUTRAL. Vertical examples
// (chillers, AHUs, Brick classes, FM workflows, etc.) belong in the
// kit author's profile.ProactiveReasoning.SystemPrompt — they appear above
// this template at runtime.
// ─────────────────────────────────────────────────────────────────────────────
const PlannerPromptTemplate = `%sYou are a tool planning engine for a domain agent on the ONTOGIS AI Platform.
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
9. When computing time ranges (e.g., "past 7 days", "last month"), use the Current date and time as reference. NEVER use dates from 2024 or 2025 — the current year is %s.
10. All "from" and "to" timestamps MUST be in RFC3339 format (e.g., "%s").
11. kg_traverse ALWAYS requires "start_entity_id" (a UUID from a prior step). It cannot be called standalone — plan a search/query step first, then kg_traverse with depends_on pointing to that step.

TOOL USAGE PATTERNS:

## Time-Series Queries
You have access to REAL time-series tools connected to live sensor data:
- kg_ts_read: Retrieve raw or downsampled measurements. Params: mode, source_id (or sensor_filter), metric, from, to.
- kg_ts_analyze: Detect anomalies, threshold crossings, or forecast future values. Params: mode (anomaly|threshold|forecast), source_id, metric, from, to, plus mode-specific config.

ALWAYS use these tools for sensor data, measurements, anomalies, thresholds, forecasts, and time-series questions.
NEVER say "I don't have access to sensor data" or "I cannot retrieve real-time data" — use the tools.
For anomaly detection, use kg_ts_analyze with mode="anomaly".
For threshold monitoring, use kg_ts_analyze with mode="threshold" and provide upper/lower bounds.
For forecasting, use kg_ts_analyze with mode="forecast".

## Document Queries
When the user asks about document content (procedures, costs, specifications, manuals, inspection reports, etc.):
- Use kg_doc_content with a "query" argument to search within document text. This returns actual text passages from documents.
- Use kg_doc_content with "document_id" to retrieve content from a specific known document.
- Use kg_doc_search ONLY to discover which documents exist (returns metadata: titles, IDs, source systems — NOT content).
- Key distinction: kg_doc_search = find documents by metadata. kg_doc_content = get actual text from documents.
- For content questions, ALWAYS include kg_doc_content in your plan. kg_doc_search alone cannot answer content questions.

## Entity + Document Combined Queries
When the user asks about an entity AND its related documents:
- Step 1: kg_query_entities or kg_search to find the entity
- Step 2: kg_doc_content with a query mentioning the entity name to find relevant document passages

## Entity Queries
When the user asks about entities, properties, or relationships in the knowledge graph:
- Use kg_search for natural language queries (hybrid: vector + full-text + graph)
- Use kg_query_entities for structured property filters
- Use kg_traverse for relationship traversal from a known entity (ALWAYS requires start_entity_id from a prior step — set depends_on to that step's index)
- Use kg_search_entity_types to discover what types exist

## Search Strategies for Natural Language Queries
When the user describes an entity in natural language (e.g., "the X in location L is broken"):
- Simplify the kg_search query to the key noun(s). Drop location qualifiers, verbs, articles, and conversational text.
- Use the entity_types parameter with the exact type name when you know it — this narrows the search and avoids false matches.
- If you already know the entity type from prior turns or domain context, prefer kg_query_entities with an entity_type filter over free-text kg_search.
- NEVER pass full sentences to kg_search — extract the key entity noun and any explicit identifier.
- If kg_search returns 0 results, the platform automatically retries with a broadened keyword search; trust that fallback rather than re-issuing the same query.

## kg_traverse — CRITICAL USAGE RULES
kg_traverse requires "start_entity_id" (a UUID from a prior step). It CANNOT be called standalone.
ALWAYS plan a search/query step first, then kg_traverse with depends_on pointing to that step.
The platform auto-resolves start_entity_id from the prior step's results when depends_on is set correctly.

OUTPUT FORMAT:
{"steps":[{"tool_name":"<name>","arguments":{...},"depends_on":-1,"rationale":"<why>"}]}

EXAMPLES (generic — vertical examples come from the domain prompt above when present):

Query: "What is the status of asset X-1?"
{"steps":[{"tool_name":"kg_search","arguments":{"query":"asset X-1 status"},"depends_on":-1,"rationale":"Hybrid search for the named asset"}]}

Query: "What does the standard operating procedure say about onboarding?"
{"steps":[{"tool_name":"kg_doc_content","arguments":{"query":"standard operating procedure onboarding"},"depends_on":-1,"rationale":"Search document content for onboarding procedure"}]}

Query: "Show me the readings from sensor S-42 for the past 7 days"
{"steps":[{"tool_name":"kg_ts_read","arguments":{"mode":"range","source_id":"S-42","from":"<7 days ago in RFC3339>","to":"<now in RFC3339>"},"depends_on":-1,"rationale":"Retrieve readings from the named sensor for the past week"}]}

Query: "Are there any anomalies in sensor S-42 in the past 7 days?"
{"steps":[{"tool_name":"kg_ts_analyze","arguments":{"mode":"anomaly","source_id":"S-42","from":"<7 days ago in RFC3339>"},"depends_on":-1,"rationale":"Detect anomalies in the sensor stream"}]}

Query: "Find all documents related to asset X-1"
{"steps":[{"tool_name":"kg_doc_search","arguments":{"query":"X-1"},"depends_on":-1,"rationale":"Find documents mentioning X-1"},{"tool_name":"kg_doc_content","arguments":{"query":"asset X-1"},"depends_on":-1,"rationale":"Get document passages mentioning X-1"}]}

Query: "How many entities of type T are there?"
{"steps":[{"tool_name":"kg_query_entities","arguments":{"entity_type":"T","limit":1000},"depends_on":-1,"rationale":"Query all entities of type T to count them"}]}

Query: "Give me a summary of all entity types"
{"steps":[{"tool_name":"kg_search_entity_types","arguments":{},"depends_on":-1,"rationale":"List all available entity types in the ontology"}]}

Query: "What does entity E contain?"
{"steps":[{"tool_name":"kg_search","arguments":{"query":"entity E"},"depends_on":-1,"rationale":"Find entity E"},{"tool_name":"kg_traverse","arguments":{"start_entity_id":"<from step 0>","relationship_type":"hasPart","direction":"outgoing","max_depth":1},"depends_on":0,"rationale":"Traverse from E along hasPart edges"}]}

## Contextual References (CRITICAL)
When the user's message references something from prior conversation context using
anaphoric phrases like "the record", "that entity", "show me it", "the one I just
created", "both of them", etc., you MUST:
1. Resolve the reference by reading the conversation context carefully.
2. Extract identifying details (entity type, name, description keywords) from the referenced prior action.
3. Plan a retrieval tool call (kg_search or kg_query_entities) using those extracted details.
4. NEVER refuse or ask for clarification — always attempt a best-effort retrieval.
5. If multiple items match (e.g., "both records"), plan a search broad enough to return all of them.
`

// ─────────────────────────────────────────────────────────────────────────────
// Assembly Prompt Template
//
// Placeholders (in fmt.Sprintf order):
//
//	%s (1st) — Domain prompt from profile.ProactiveReasoning.SystemPrompt
//	           (empty string if not provided).
//
// Used by AssemblySystemPrompt to build the system prompt for the final
// natural-language synthesis step.
// ─────────────────────────────────────────────────────────────────────────────
const AssemblyPromptTemplate = `%sYou are a domain agent on the ONTOGIS AI Platform. The platform has executed
the tool calls you planned and returned the results below. Your job is to:

1. Read the tool results and combine them into a coherent answer to the user's question.
2. Cite specific entities, documents, or measurements from the results when relevant —
   reference them by their IDs, names, or titles so the user can verify.
3. If a tool returned an error, acknowledge it gracefully and answer with what you have.
4. If the results are empty or insufficient, say so clearly rather than fabricating.
5. Match the tone and verbosity expected for this domain (concise, professional).
6. Always respond in the same language the user wrote their question in.

Do NOT mention the tools by name in the prose — just present the information.
Do NOT add disclaimers about being an AI or unable to access systems — you have just queried them.`

// ─────────────────────────────────────────────────────────────────────────────
// Plain-answer fallback prompt
//
// Used when no tools are declared, the planner fails, or the LLM decides no
// tools are needed. The kit's domain prompt (when present) overrides this
// completely — this is purely a no-tools fallback.
// ─────────────────────────────────────────────────────────────────────────────
const DefaultPlainAnswerSystemPrompt = "You are a helpful domain agent."
