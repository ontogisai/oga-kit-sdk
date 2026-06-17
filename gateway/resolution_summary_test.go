package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestApprovalResolvedEvent_ResolutionSummaryOmittedWhenNil is the
// backward-compatibility guard (OGA-382): an event without a summary must NOT
// emit a resolution_summary key, so older consumers see no shape change.
func TestApprovalResolvedEvent_ResolutionSummaryOmittedWhenNil(t *testing.T) {
	ev := ApprovalResolvedEvent{
		EventType:  EventTypeActionResolved,
		TenantID:   "sgac1",
		ProposalID: "p-1",
		Status:     ApprovalStatusRejected,
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "resolution_summary") {
		t.Errorf("nil ResolutionSummary must be omitted; got %s", b)
	}
}

// TestResolutionSummary_RoundTrip verifies a fully-populated summary survives a
// JSON round-trip on the event with all sub-types intact.
func TestResolutionSummary_RoundTrip(t *testing.T) {
	in := ApprovalResolvedEvent{
		EventType:       EventTypeActionResolved,
		TenantID:        "sgac1",
		ProposalID:      "p-42",
		Status:          ApprovalStatusApproved,
		ExecutionStatus: "executed",
		ResolutionSummary: &ResolutionSummary{
			Status:          "approved",
			ExecutionStatus: "executed",
			Headline:        "resolution.headline.approved",
			DecidedBy:       "user-1",
			CreatedEntities: []ResolvedEntityRef{{
				EntityID:   "wo-1",
				EntityType: "sgac1_WorkOrder",
				Label:      "WO-1234",
				KeyProps:   map[string]string{"assignee": "FM Team", "due_date": "2026-06-18"},
				DeepLink:   "/entities/wo-1",
			}},
			ExternalRecords: []ExternalRef{{System: "maximo", RecordID: "WO-1234", URL: "https://maximo/WO-1234"}},
			FollowUps: []FollowUpHint{{
				Key:  "resolution.followup.assigned_due",
				Args: map[string]string{"assignee": "FM Team", "due": "2026-06-18"},
			}},
		},
	}

	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out ApprovalResolvedEvent
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	rs := out.ResolutionSummary
	if rs == nil {
		t.Fatal("ResolutionSummary lost in round-trip")
	}
	if rs.Status != "approved" || rs.ExecutionStatus != "executed" || rs.Headline != "resolution.headline.approved" {
		t.Errorf("scalar fields mismatch: %+v", rs)
	}
	if len(rs.CreatedEntities) != 1 || rs.CreatedEntities[0].EntityType != "sgac1_WorkOrder" ||
		rs.CreatedEntities[0].KeyProps["assignee"] != "FM Team" {
		t.Errorf("created entity mismatch: %+v", rs.CreatedEntities)
	}
	if len(rs.ExternalRecords) != 1 || rs.ExternalRecords[0].RecordID != "WO-1234" {
		t.Errorf("external record mismatch: %+v", rs.ExternalRecords)
	}
	if len(rs.FollowUps) != 1 || rs.FollowUps[0].Key != "resolution.followup.assigned_due" ||
		rs.FollowUps[0].Args["due"] != "2026-06-18" {
		t.Errorf("follow-up mismatch: %+v", rs.FollowUps)
	}
}

// TestResolutionSummary_FailedHasNoCreatedEntity guards the real-data-only
// property (P1): a failed execution carries the error reason and no fabricated
// created-entity.
func TestResolutionSummary_FailedHasNoCreatedEntity(t *testing.T) {
	rs := ResolutionSummary{
		Status:          "approved",
		ExecutionStatus: "failed",
		Reason:          "integration timeout",
	}
	if len(rs.CreatedEntities) != 0 {
		t.Error("failed execution must not carry a created entity")
	}
	b, _ := json.Marshal(rs)
	if strings.Contains(string(b), "created_entities") {
		t.Errorf("failed summary must omit created_entities; got %s", b)
	}
}
