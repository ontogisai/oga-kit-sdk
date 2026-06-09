package streampipeline

import (
	"encoding/json"
	"fmt"

	"github.com/ontogisai/oga-kit-sdk/agent"
)

// MaxCitationsPerStep caps the number of entity citations extracted from a
// single tool result. Lifted from the platform's Knowledge Agent constant.
const MaxCitationsPerStep = 10

// ExtractCitations builds citation sources from a tool-step result + its
// arguments. This is the unified extractor used by both Knowledge Agent and
// kit-supplied domain agents — same JSON shape, same rules.
//
// Citation rules (in order):
//  1. Entity citations parsed from {"results": [{id, entity_id, name,
//     entity_type}, ...]} or singleton {entity_id, name, entity_type}.
//     Capped at MaxCitationsPerStep.
//  2. Document citations parsed from {"passages": [{document_id,
//     document_name, ...}, ...]} (the shape returned by kg_doc_content
//     and kg_doc_search). Deduplicated per document_id, capped at
//     MaxCitationsPerStep.
//  3. Spatial citation when args carry "h3_cells" (the tool was scoped to
//     specific H3 cells).
//  4. Temporal citation when args carry "valid_from" or "valid_to" (the
//     tool was scoped to a bi-temporal range).
//  5. Generic fallback citation when nothing specific was extracted —
//     records that the response was grounded in a tool query.
//
// Returns nil if the result is not successful or has empty content.
func ExtractCitations(result *ToolStepResult, toolName string, args map[string]any) []agent.CitationSource {
	if result == nil || !result.Success || result.Content == "" {
		return nil
	}

	var citations []agent.CitationSource

	// 1. Entity citations from result content.
	entityCitations := ExtractEntityCitations(result.Content)
	citations = append(citations, entityCitations...)

	// 2. Document citations from result content. Document tools (e.g.
	// kg_doc_content, kg_doc_search) return passages keyed by document_id;
	// the parser is content-shape-driven rather than tool-name-driven so
	// any tool whose result follows the passages shape produces document
	// chips automatically.
	documentCitations := ExtractDocumentCitations(result.Content)
	citations = append(citations, documentCitations...)

	// 3. Spatial citations from args.
	if h3Cells, ok := args["h3_cells"].([]any); ok && len(h3Cells) > 0 {
		cells := make([]string, 0, len(h3Cells))
		for _, c := range h3Cells {
			if s, ok := c.(string); ok {
				cells = append(cells, s)
			}
		}
		if len(cells) > 0 {
			citations = append(citations, agent.CitationSource{
				Type:    "h3_cells",
				ID:      "spatial:" + toolName,
				Label:   "Spatial query via " + toolName,
				H3Cells: cells,
			})
		}
	}

	// 4. Temporal citations from args.
	validFrom, _ := args["valid_from"].(string)
	validTo, _ := args["valid_to"].(string)
	if validFrom != "" || validTo != "" {
		citations = append(citations, agent.CitationSource{
			Type:      "time_range",
			ID:        fmt.Sprintf("temporal:%s:%d", toolName, result.StepIndex),
			Label:     "Temporal query via " + toolName,
			ValidFrom: validFrom,
			ValidTo:   validTo,
		})
	}

	// 5. Generic fallback if nothing else was extracted.
	if len(citations) == 0 {
		citations = append(citations, agent.CitationSource{
			Type:  "entity",
			ID:    fmt.Sprintf("tool:%s:%d", toolName, result.StepIndex),
			Label: "Knowledge graph query via " + toolName,
		})
	}

	return citations
}

// ExtractEntityCitations attempts to parse entity references from a tool's
// JSON result content. Handles two common shapes:
//  1. {"results": [{"id"|"entity_id": ..., "name": ..., "entity_type": ...}, ...]}
//  2. Single entity: {"entity_id": ..., "name": ..., "entity_type": ...}
//
// Returns nil for unparseable content. Caps at MaxCitationsPerStep.
func ExtractEntityCitations(content string) []agent.CitationSource {
	// Shape 1: results array
	var arrayShape struct {
		Results []struct {
			ID         string `json:"id"`
			EntityID   string `json:"entity_id"`
			Name       string `json:"name"`
			EntityType string `json:"entity_type"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(content), &arrayShape); err == nil && len(arrayShape.Results) > 0 {
		citations := make([]agent.CitationSource, 0, len(arrayShape.Results))
		for _, r := range arrayShape.Results {
			id := r.ID
			if id == "" {
				id = r.EntityID
			}
			if id == "" {
				continue
			}
			label := r.Name
			if label == "" && r.EntityType != "" {
				label = r.EntityType + ":" + id
			}
			if label == "" {
				label = id
			}
			citations = append(citations, agent.CitationSource{
				Type:  "entity",
				ID:    id,
				Label: label,
			})
			if len(citations) >= MaxCitationsPerStep {
				break
			}
		}
		return citations
	}

	// Shape 2: single entity
	var singleShape struct {
		ID         string `json:"id"`
		EntityID   string `json:"entity_id"`
		Name       string `json:"name"`
		EntityType string `json:"entity_type"`
	}
	if err := json.Unmarshal([]byte(content), &singleShape); err == nil {
		id := singleShape.ID
		if id == "" {
			id = singleShape.EntityID
		}
		if id != "" {
			label := singleShape.Name
			if label == "" && singleShape.EntityType != "" {
				label = singleShape.EntityType + ":" + id
			}
			if label == "" {
				label = id
			}
			return []agent.CitationSource{{
				Type:  "entity",
				ID:    id,
				Label: label,
			}}
		}
	}

	return nil
}

// ExtractDocumentCitations attempts to parse document references from a
// tool's JSON result content. Targets the
// `oga-platform/internal/mcptoolserver.DocContentResponse` shape produced
// by `kg_doc_content` and `kg_doc_search`:
//
//	{ "passages": [{ "document_id": "...", "document_name": "...", ... }] }
//
// Per-document deduplication: passages from the same document only emit one
// citation chip. Empty `document_name` falls back to `document_id` so the
// label is never empty. Caps at MaxCitationsPerStep, mirroring the entity
// extractor's safeguard against oversized events.
//
// Returns nil for unparseable content or content that does not match the
// passages shape.
func ExtractDocumentCitations(content string) []agent.CitationSource {
	var parsed struct {
		Passages []struct {
			DocumentID   string `json:"document_id"`
			DocumentName string `json:"document_name"`
		} `json:"passages"`
	}
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		return nil
	}

	var citations []agent.CitationSource
	seen := make(map[string]struct{}, len(parsed.Passages))
	for _, p := range parsed.Passages {
		if p.DocumentID == "" {
			continue
		}
		if _, dup := seen[p.DocumentID]; dup {
			continue
		}
		seen[p.DocumentID] = struct{}{}

		label := p.DocumentName
		if label == "" {
			label = p.DocumentID
		}
		citations = append(citations, agent.CitationSource{
			Type:  "document",
			ID:    p.DocumentID,
			Label: label,
		})
		if len(citations) >= MaxCitationsPerStep {
			break
		}
	}

	return citations
}
