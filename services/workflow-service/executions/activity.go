package executions

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"time"

	"go.temporal.io/sdk/temporal"
)

type Capability struct {
	ID                     string `json:"id"`
	AgentID                string `json:"agentId"`
	AgentVersion           string `json:"agentVersion"`
	RequiresMatter         bool   `json:"requiresMatter"`
	RequiresAttorneyReview bool   `json:"requiresAttorneyReview"`
	MaxInputBytes          int    `json:"maxInputBytes"`
}
type Registry struct {
	RegistryVersion string       `json:"registryVersion"`
	Capabilities    []Capability `json:"capabilities"`
}

func LoadRegistry(path string) (map[string]Capability, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var registry Registry
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&registry); err != nil {
		return nil, err
	}
	if registry.RegistryVersion != "1.0" || len(registry.Capabilities) == 0 {
		return nil, errors.New("invalid registry")
	}
	result := map[string]Capability{}
	pattern := regexp.MustCompile(`^[a-z][a-z0-9.]*\.v[1-9]\d*$`)
	for _, cap := range registry.Capabilities {
		if _, ok := result[cap.ID]; ok || !pattern.MatchString(cap.ID) || cap.MaxInputBytes < 1 || cap.MaxInputBytes > 32768 || cap.AgentID == "" || cap.AgentVersion == "" {
			return nil, errors.New("invalid capability")
		}
		result[cap.ID] = cap
	}
	return result, nil
}

type Evidence struct {
	DocumentID        string `json:"documentId"`
	DocumentVersionID string `json:"documentVersionId"`
	PassageID         string `json:"passageId"`
	PageNumber        *int   `json:"pageNumber,omitempty"`
}
type Result struct {
	ExecutionID            string                     `json:"executionId"`
	AgentID                string                     `json:"agentId"`
	AgentVersion           string                     `json:"agentVersion"`
	Status                 string                     `json:"status"`
	Output                 map[string]json.RawMessage `json:"output"`
	Evidence               []Evidence                 `json:"evidence"`
	ProposedActions        []json.RawMessage          `json:"proposedActions"`
	RequiresAttorneyReview *bool                      `json:"requiresAttorneyReview"`
}

func ValidateResult(raw []byte, e Execution, cap Capability) (Result, error) {
	var result Result
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&result); err != nil {
		return result, errors.New("INVALID_RESULT")
	}
	if dec.Decode(&struct{}{}) != io.EOF || result.ExecutionID != e.ExecutionID || result.AgentID != cap.AgentID || result.AgentVersion != cap.AgentVersion || result.Output == nil || result.Evidence == nil || result.ProposedActions == nil || result.RequiresAttorneyReview == nil || len(result.ProposedActions) > 0 || cap.RequiresAttorneyReview && !*result.RequiresAttorneyReview {
		return result, errors.New("INVALID_RESULT")
	}
	if result.Status != "completed" && result.Status != "abstained" && result.Status != "failed" {
		return result, errors.New("INVALID_RESULT")
	}
	if result.Status == "completed" && cap.RequiresMatter && len(result.Evidence) == 0 {
		return result, errors.New("INVALID_RESULT")
	}
	for _, ref := range result.Evidence {
		if ref.DocumentID == "" || ref.DocumentVersionID == "" || ref.PassageID == "" || (ref.PageNumber != nil && *ref.PageNumber < 1) {
			return result, errors.New("INVALID_RESULT")
		}
	}
	return result, nil
}

type Activities struct {
	Store         *Store
	Registry      map[string]Capability
	SupervisorURL string
	Secret        string
	HTTP          *http.Client
}

func permanent(code string) error { return temporal.NewNonRetryableApplicationError(code, code, nil) }
func (a *Activities) Run(ctx context.Context, ref Ref) (string, error) {
	e, err := a.Store.Begin(ctx, ref)
	if errors.Is(err, ErrPermission) {
		return "", permanent("PERMISSION_DENIED")
	}
	if err != nil {
		return "", errors.New("DATABASE_UNAVAILABLE")
	}
	if terminal(e.Status) {
		return e.Status, nil
	}
	cap, ok := a.Registry[e.CapabilityID]
	if !ok {
		return "", permanent("UNSUPPORTED_CAPABILITY")
	}
	var compact bytes.Buffer
	if err = json.Compact(&compact, e.Input); err != nil {
		return "", permanent("INVALID_REQUEST")
	}
	e.Input = compact.Bytes()
	if cap.RequiresMatter && e.MatterID == nil || len(e.Input) > cap.MaxInputBytes {
		return "", permanent("INVALID_REQUEST")
	}
	task := map[string]any{"executionId": e.ExecutionID, "tenantId": e.TenantID, "principalId": e.PrincipalID, "capabilityId": e.CapabilityID, "input": json.RawMessage(e.Input), "traceId": e.ExecutionID}
	if e.MatterID != nil {
		task["matterId"] = *e.MatterID
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	err = encoder.Encode(task)
	body := encoded.Bytes()
	if err != nil {
		return "", permanent("INVALID_REQUEST")
	}
	req, err := http.NewRequestWithContext(ctx, "POST", a.SupervisorURL+"/internal/execute", bytes.NewReader(body))
	if err != nil {
		return "", permanent("INVALID_CONFIGURATION")
	}
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(a.Secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Lawpilot-Timestamp", ts)
	req.Header.Set("X-Lawpilot-Signature", hex.EncodeToString(mac.Sum(nil)))
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return "", errors.New("SUPERVISOR_UNAVAILABLE")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 || resp.StatusCode >= 500 {
		return "", errors.New("SUPERVISOR_UNAVAILABLE")
	}
	if resp.StatusCode != 200 {
		return "", permanent("SUPERVISOR_REJECTED")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1_048_577))
	if err != nil {
		return "", errors.New("SUPERVISOR_UNAVAILABLE")
	}
	if len(raw) > 1_048_576 {
		return "", permanent("INVALID_RESULT")
	}
	result, err := ValidateResult(raw, e, cap)
	if err != nil {
		return "", permanent("INVALID_RESULT")
	}
	if result.Status == "failed" {
		return "", permanent("AI_FAILED")
	}
	state := "completed"
	if result.Status == "completed" && cap.RequiresAttorneyReview {
		state = "awaiting_review"
	}
	state, err = a.Store.Finish(ctx, ref, state, result.Status, raw, true)
	if errors.Is(err, ErrPermission) {
		return "", permanent("PERMISSION_DENIED")
	}
	if err != nil {
		return "", fmt.Errorf("DATABASE_UNAVAILABLE")
	}
	return state, nil
}

type Failure struct {
	Ref  Ref
	Code string
}

func (a *Activities) Fail(ctx context.Context, in Failure) (string, error) {
	return a.Store.Finish(ctx, in.Ref, "failed", in.Code, nil, false)
}
