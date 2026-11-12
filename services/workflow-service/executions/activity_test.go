package executions

import (
	"encoding/json"
	"testing"
)

func TestResultPolicyCannotBeBypassed(t *testing.T) {
	cap := Capability{ID: "intake.summarize.v1", AgentID: "intake-agent", AgentVersion: "0.1.0", RequiresAttorneyReview: true}
	e := Execution{Ref: Ref{ExecutionID: "execution"}}
	result := map[string]any{"executionId": "execution", "agentId": "intake-agent", "agentVersion": "0.1.0", "status": "completed", "output": map[string]any{}, "evidence": []any{}, "proposedActions": []any{}, "requiresAttorneyReview": true}
	raw, _ := json.Marshal(result)
	if _, err := ValidateResult(raw, e, cap); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]any{"executionId": "other", "agentId": "other", "requiresAttorneyReview": false, "proposedActions": []any{map[string]any{"write": true}}} {
		altered := map[string]any{}
		for k, v := range result {
			altered[k] = v
		}
		altered[key] = value
		raw, _ := json.Marshal(altered)
		if _, err := ValidateResult(raw, e, cap); err == nil {
			t.Fatalf("accepted invalid %s", key)
		}
	}
	cap.RequiresMatter = true
	if _, err := ValidateResult(raw, e, cap); err == nil {
		t.Fatal("accepted matter result without evidence")
	}
}
