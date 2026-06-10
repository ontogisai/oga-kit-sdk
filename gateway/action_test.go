package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func validInput() *SubmitActionInput {
	return &SubmitActionInput{
		ActionName:      "create_work_order",
		Payload:         map[string]any{"priority": "P2"},
		Description:     "Create a work order",
		Reasoning:       "sensor trend exceeded threshold",
		ExpectedOutcome: "WO created",
		Routing:         ActionRouting{TargetUserID: "op-1"},
		HumanActionMode: HumanActionModeApproval,
		RiskLevel:       RiskLevelMedium,
	}
}

func TestSubmitAction_Validation(t *testing.T) {
	c := NewPlatformGatewayClient("http://localhost:0", "", "sgac1")
	cases := []struct {
		name string
		mut  func(*SubmitActionInput)
	}{
		{"missing action_name", func(in *SubmitActionInput) { in.ActionName = "" }},
		{"bad human_action_mode", func(in *SubmitActionInput) { in.HumanActionMode = "maybe" }},
		{"bad risk_level", func(in *SubmitActionInput) { in.RiskLevel = "extreme" }},
		{"no routing target", func(in *SubmitActionInput) { in.Routing = ActionRouting{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validInput()
			tc.mut(in)
			_, err := c.SubmitAction(context.Background(), in)
			if !errors.Is(err, ErrInvalidActionInput) {
				t.Fatalf("expected ErrInvalidActionInput, got %v", err)
			}
		})
	}
	t.Run("nil input", func(t *testing.T) {
		if _, err := c.SubmitAction(context.Background(), nil); !errors.Is(err, ErrInvalidActionInput) {
			t.Fatalf("expected ErrInvalidActionInput, got %v", err)
		}
	})
}

func TestSubmitActionProposal_PostsAndGeneratesIDs(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/workflow" {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]string{"workflow_id": "wf-123"})
	}))
	defer srv.Close()

	// Token file (loadToken reads it for the Authorization header).
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("test-token"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	c := NewPlatformGatewayClient(srv.URL, tokenPath, "sgac1")
	sub, err := c.SubmitActionProposal(context.Background(), &ActionProposal{
		ActionType:      "create_work_order",
		HumanActionMode: HumanActionModeApproval,
		RiskLevel:       RiskLevelMedium,
		Routing:         ActionRouting{TargetUserID: "op-1"},
	})
	if err != nil {
		t.Fatalf("SubmitActionProposal: %v", err)
	}
	if sub.WorkflowID != "wf-123" {
		t.Errorf("WorkflowID = %q, want wf-123", sub.WorkflowID)
	}
	if sub.ProposalID == "" {
		t.Error("ProposalID should be generated when empty")
	}
	if gotBody["type"] != WorkflowTypeAgentApproval {
		t.Errorf("posted type = %v, want %s", gotBody["type"], WorkflowTypeAgentApproval)
	}
}
