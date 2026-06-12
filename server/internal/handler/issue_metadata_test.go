package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Round-trip: set primitives of each type, list, get them back, delete, confirm gone.
func TestIssueMetadataSetGetDelete(t *testing.T) {
	issueID := createMetadataTestIssue(t, "Metadata round-trip")

	cases := []struct {
		key   string
		value string // raw JSON value
	}{
		{"pipeline_status", `"waiting"`},
		{"pr_number", `482`},
		{"is_blocked", `true`},
		{"is_done", `false`},
	}

	for _, c := range cases {
		w := httptest.NewRecorder()
		req := newRequest("PUT", "/api/issues/"+issueID+"/metadata/"+c.key, json.RawMessage(`{"value":`+c.value+`}`))
		req = withURLParams(req, "id", issueID, "key", c.key)
		testHandler.SetIssueMetadataKey(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("Set %s=%s: expected 200, got %d: %s", c.key, c.value, w.Code, w.Body.String())
		}
	}

	// List returns every key with the right value type.
	w := httptest.NewRecorder()
	req := newRequest("GET", "/api/issues/"+issueID+"/metadata", nil)
	req = withURLParam(req, "id", issueID)
	testHandler.ListIssueMetadata(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("List metadata: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if got := resp.Metadata["pipeline_status"]; got != "waiting" {
		t.Errorf("pipeline_status: expected \"waiting\", got %T %v", got, got)
	}
	if got := resp.Metadata["pr_number"]; got != float64(482) {
		t.Errorf("pr_number: expected number 482, got %T %v", got, got)
	}
	if got := resp.Metadata["is_blocked"]; got != true {
		t.Errorf("is_blocked: expected true, got %T %v", got, got)
	}
	if got := resp.Metadata["is_done"]; got != false {
		t.Errorf("is_done: expected false, got %T %v", got, got)
	}

	// Delete a key — refresh confirms it is gone, others remain.
	w = httptest.NewRecorder()
	req = newRequest("DELETE", "/api/issues/"+issueID+"/metadata/pipeline_status", nil)
	req = withURLParams(req, "id", issueID, "key", "pipeline_status")
	testHandler.DeleteIssueMetadataKey(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Delete pipeline_status: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	req = newRequest("GET", "/api/issues/"+issueID+"/metadata", nil)
	req = withURLParam(req, "id", issueID)
	testHandler.ListIssueMetadata(w, req)
	// Decode into a fresh struct — json.Decode into a non-nil map merges,
	// it does not replace, so reusing `resp` would keep deleted keys around.
	var afterDelete struct {
		Metadata map[string]any `json:"metadata"`
	}
	json.NewDecoder(w.Body).Decode(&afterDelete)
	if _, present := afterDelete.Metadata["pipeline_status"]; present {
		t.Errorf("after delete, pipeline_status should be gone; got %+v", afterDelete.Metadata)
	}
	if _, present := afterDelete.Metadata["pr_number"]; !present {
		t.Errorf("delete removed unrelated key; got %+v", afterDelete.Metadata)
	}
}

// Invalid keys / values / shapes are rejected with 400 — the regex, primitive,
// and "no null" rules must all hold.
func TestIssueMetadataValidation(t *testing.T) {
	issueID := createMetadataTestIssue(t, "Metadata validation")

	bad := []struct {
		name    string
		key     string
		rawBody string
	}{
		{"key starts with digit", "1attempts", `{"value":"x"}`},
		{"key has space", "foo bar", `{"value":"x"}`},
		{"value is null", "k", `{"value":null}`},
		{"value is array", "k", `{"value":[1,2]}`},
		{"value is object", "k", `{"value":{"a":1}}`},
		{"empty body", "k", ``},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			// chi pulls the key from URL params (injected via withURLParams);
			// the raw URL needs to be a valid request line, so PathEscape any
			// chars (spaces, etc.) that would otherwise break httptest.NewRequest.
			req := newRequest("PUT", "/api/issues/"+issueID+"/metadata/"+url.PathEscape(c.key), json.RawMessage(c.rawBody))
			req = withURLParams(req, "id", issueID, "key", c.key)
			testHandler.SetIssueMetadataKey(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// The 8KB DB CHECK kicks in past a few hundred KV pairs of large strings; we
// blow it deliberately with one giant value to confirm the handler surfaces
// a 400 (not a generic 500).
func TestIssueMetadataSizeLimit(t *testing.T) {
	issueID := createMetadataTestIssue(t, "Metadata size limit")

	huge := strings.Repeat("a", 9000)
	body, _ := json.Marshal(map[string]any{"value": huge})
	w := httptest.NewRecorder()
	req := newRequest("PUT", "/api/issues/"+issueID+"/metadata/blob", body)
	req = withURLParams(req, "id", issueID, "key", "blob")
	testHandler.SetIssueMetadataKey(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 from size CHECK, got %d: %s", w.Code, w.Body.String())
	}
}

// The 50-key cap is enforced in the handler with a clear 400.
func TestIssueMetadataKeyCountCap(t *testing.T) {
	issueID := createMetadataTestIssue(t, "Metadata key count cap")

	for i := 0; i < maxIssueMetadataKeys; i++ {
		key := fmt.Sprintf("k_%d", i)
		w := httptest.NewRecorder()
		req := newRequest("PUT", "/api/issues/"+issueID+"/metadata/"+key, json.RawMessage(`{"value":"v"}`))
		req = withURLParams(req, "id", issueID, "key", key)
		testHandler.SetIssueMetadataKey(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("key #%d: expected 200, got %d: %s", i, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	req := newRequest("PUT", "/api/issues/"+issueID+"/metadata/overflow", json.RawMessage(`{"value":"v"}`))
	req = withURLParams(req, "id", issueID, "key", "overflow")
	testHandler.SetIssueMetadataKey(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("overflow key: expected 400, got %d: %s", w.Code, w.Body.String())
	}

	// Updating an existing key past the cap is still allowed — only new keys
	// are blocked.
	w = httptest.NewRecorder()
	req = newRequest("PUT", "/api/issues/"+issueID+"/metadata/k_0", json.RawMessage(`{"value":"v2"}`))
	req = withURLParams(req, "id", issueID, "key", "k_0")
	testHandler.SetIssueMetadataKey(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update existing at cap: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// ListIssues with `metadata` query param does JSONB containment filtering and
// returns only matching issues — the killer use case for autopilot.
func TestListIssuesMetadataFilter(t *testing.T) {
	waitingID := createMetadataTestIssue(t, "Waiting issue")
	doneID := createMetadataTestIssue(t, "Done issue")

	for issueID, status := range map[string]string{waitingID: "waiting_review", doneID: "deployed"} {
		w := httptest.NewRecorder()
		req := newRequest("PUT", "/api/issues/"+issueID+"/metadata/pipeline_status",
			json.RawMessage(`{"value":"`+status+`"}`))
		req = withURLParams(req, "id", issueID, "key", "pipeline_status")
		testHandler.SetIssueMetadataKey(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("seed %s: %d %s", issueID, w.Code, w.Body.String())
		}
	}

	w := httptest.NewRecorder()
	req := newRequest("GET", `/api/issues?metadata={"pipeline_status":"waiting_review"}`, nil)
	testHandler.ListIssues(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("List with filter: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var listResp struct {
		Issues []IssueResponse `json:"issues"`
	}
	json.NewDecoder(w.Body).Decode(&listResp)

	foundWaiting := false
	for _, iss := range listResp.Issues {
		if iss.ID == doneID {
			t.Errorf("filter leaked: deployed issue %s appeared in waiting_review result set", doneID)
		}
		if iss.ID == waitingID {
			foundWaiting = true
			if got, _ := iss.Metadata["pipeline_status"].(string); got != "waiting_review" {
				t.Errorf("waiting issue: pipeline_status not surfaced; got %v", iss.Metadata)
			}
		}
	}
	if !foundWaiting {
		t.Errorf("waiting issue %s missing from filter result; got %d issues", waitingID, len(listResp.Issues))
	}

	// Malformed filter → 400.
	w = httptest.NewRecorder()
	req = newRequest("GET", `/api/issues?metadata={not-json}`, nil)
	testHandler.ListIssues(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed metadata: expected 400, got %d", w.Code)
	}
}

// New issues default to an empty metadata object — never null — so frontend
// reads like `issue.metadata[key]` never NPE.
func TestNewIssueDefaultsToEmptyMetadata(t *testing.T) {
	issueID := createMetadataTestIssue(t, "Default empty metadata")

	w := httptest.NewRecorder()
	req := newRequest("GET", "/api/issues/"+issueID, nil)
	req = withURLParam(req, "id", issueID)
	testHandler.GetIssue(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GetIssue: %d %s", w.Code, w.Body.String())
	}
	var got IssueResponse
	json.NewDecoder(w.Body).Decode(&got)
	if got.Metadata == nil {
		t.Fatalf("Metadata is nil on a fresh issue; expected empty object")
	}
	if len(got.Metadata) != 0 {
		t.Fatalf("Metadata: expected empty, got %v", got.Metadata)
	}
}

func TestIssueMetadataWaitingReviewDispatchesReviewer(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	reviewerID := createNamedHandlerTestAgent(t, "Reviewer")
	issueID := createMetadataTestIssue(t, "Waiting for review gate")

	w := httptest.NewRecorder()
	req := newRequest("PUT", "/api/issues/"+issueID+"/metadata/gate_review", json.RawMessage(`{"value":"pending@abc1234"}`))
	req = withURLParams(req, "id", issueID, "key", "gate_review")
	testHandler.SetIssueMetadataKey(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Set gate_review: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req = newRequest("PUT", "/api/issues/"+issueID+"/metadata/pipeline_status", json.RawMessage(`{"value":"waiting_review"}`))
	req = withURLParams(req, "id", issueID, "key", "pipeline_status")
	testHandler.SetIssueMetadataKey(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Set pipeline_status: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var taskCount int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
	`, issueID, reviewerID).Scan(&taskCount); err != nil {
		t.Fatalf("count reviewer tasks: %v", err)
	}
	if taskCount != 1 {
		t.Fatalf("expected one reviewer task after waiting_review metadata, got %d", taskCount)
	}

	var metadataRaw []byte
	if err := testPool.QueryRow(ctx, `SELECT metadata FROM issue WHERE id = $1`, issueID).Scan(&metadataRaw); err != nil {
		t.Fatalf("load issue metadata: %v", err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(metadataRaw, &metadata); err != nil {
		t.Fatalf("decode issue metadata: %v", err)
	}
	if got := metadata["gate_dispatch_status"]; got != "waiting_for_agent_run" {
		t.Fatalf("gate_dispatch_status = %v, want waiting_for_agent_run", got)
	}
	if got := metadata["gate_dispatch_head"]; got != "abc1234" {
		t.Fatalf("gate_dispatch_head = %v, want abc1234", got)
	}
}

func TestIssueMetadataPipelineBeforeGateHeadWaitsThenDispatchesOnce(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	reviewerID := createNamedHandlerTestAgent(t, "Reviewer")
	issueID := createMetadataTestIssue(t, "Pipeline before review head")

	w := httptest.NewRecorder()
	req := newRequest("PUT", "/api/issues/"+issueID+"/metadata/pipeline_status", json.RawMessage(`{"value":"waiting_review"}`))
	req = withURLParams(req, "id", issueID, "key", "pipeline_status")
	testHandler.SetIssueMetadataKey(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Set pipeline_status: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := countGateTasksForAgent(t, ctx, issueID, reviewerID); got != 0 {
		t.Fatalf("pipeline_status before gate head should not dispatch; got %d tasks", got)
	}

	w = httptest.NewRecorder()
	req = newRequest("PUT", "/api/issues/"+issueID+"/metadata/gate_review", json.RawMessage(`{"value":"pending@def5678"}`))
	req = withURLParams(req, "id", issueID, "key", "gate_review")
	testHandler.SetIssueMetadataKey(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Set gate_review: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := countGateTasksForAgent(t, ctx, issueID, reviewerID); got != 1 {
		t.Fatalf("expected exactly one reviewer task after gate head arrives, got %d", got)
	}
}

func TestIssueMetadataWaitingTestDispatchesMacOSQA(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	qaID := createNamedHandlerTestAgent(t, "QA (macOS)")
	issueID := createMetadataTestIssue(t, "Waiting for test gate")

	w := httptest.NewRecorder()
	req := newRequest("PUT", "/api/issues/"+issueID+"/metadata/gate_test", json.RawMessage(`{"value":"pending@abc1234"}`))
	req = withURLParams(req, "id", issueID, "key", "gate_test")
	testHandler.SetIssueMetadataKey(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Set gate_test: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req = newRequest("PUT", "/api/issues/"+issueID+"/metadata/pipeline_status", json.RawMessage(`{"value":"waiting_test"}`))
	req = withURLParams(req, "id", issueID, "key", "pipeline_status")
	testHandler.SetIssueMetadataKey(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Set pipeline_status: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := countGateTasksForAgent(t, ctx, issueID, qaID); got != 1 {
		t.Fatalf("expected one QA (macOS) task after waiting_test metadata, got %d", got)
	}
}

func TestIssueMetadataWaitingPRRDispatchesSRE(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	sreID := createNamedHandlerTestAgent(t, "SRE")
	issueID := createMetadataTestIssue(t, "Waiting for PRR gate")

	w := httptest.NewRecorder()
	req := newRequest("PUT", "/api/issues/"+issueID+"/metadata/gate_prr", json.RawMessage(`{"value":"pending@abc1234"}`))
	req = withURLParams(req, "id", issueID, "key", "gate_prr")
	testHandler.SetIssueMetadataKey(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Set gate_prr: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req = newRequest("PUT", "/api/issues/"+issueID+"/metadata/pipeline_status", json.RawMessage(`{"value":"waiting_prr"}`))
	req = withURLParams(req, "id", issueID, "key", "pipeline_status")
	testHandler.SetIssueMetadataKey(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Set pipeline_status: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := countGateTasksForAgent(t, ctx, issueID, sreID); got != 1 {
		t.Fatalf("expected one SRE task after waiting_prr metadata, got %d", got)
	}
}

func TestIssueMetadataGateDispatchMarksArchivedAgentFailure(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	reviewerID := createNamedHandlerTestAgent(t, "Reviewer")
	issueID := createMetadataTestIssue(t, "Archived reviewer dispatch failure")
	if _, err := testPool.Exec(ctx, `UPDATE agent SET archived_at = now() WHERE id = $1`, reviewerID); err != nil {
		t.Fatalf("archive reviewer: %v", err)
	}

	w := httptest.NewRecorder()
	req := newRequest("PUT", "/api/issues/"+issueID+"/metadata/gate_review", json.RawMessage(`{"value":"pending@abc1234"}`))
	req = withURLParams(req, "id", issueID, "key", "gate_review")
	testHandler.SetIssueMetadataKey(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Set gate_review: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req = newRequest("PUT", "/api/issues/"+issueID+"/metadata/pipeline_status", json.RawMessage(`{"value":"waiting_review"}`))
	req = withURLParams(req, "id", issueID, "key", "pipeline_status")
	testHandler.SetIssueMetadataKey(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Set pipeline_status: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	metadata := loadIssueMetadata(t, ctx, issueID)
	if got := metadata["pipeline_status"]; got != "gate_dispatch_failed" {
		t.Fatalf("pipeline_status = %v, want gate_dispatch_failed", got)
	}
	if got := metadata["gate_dispatch_status"]; got != "dispatch_failed" {
		t.Fatalf("gate_dispatch_status = %v, want dispatch_failed", got)
	}
	waitingOn, _ := metadata["waiting_on"].(string)
	if !strings.Contains(waitingOn, "agent is archived") {
		t.Fatalf("waiting_on = %q, want concrete archived-agent reason", waitingOn)
	}
}

func TestIssueMetadataGateDispatchReportsActiveExistingRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	reviewerID := createNamedHandlerTestAgent(t, "Reviewer")
	issueID := createMetadataTestIssue(t, "Existing reviewer run")
	var taskID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority)
		VALUES ($1, $2, $3, 'running', 0)
		RETURNING id
	`, reviewerID, handlerTestRuntimeID(t), issueID).Scan(&taskID); err != nil {
		t.Fatalf("seed running reviewer task: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
	})

	w := httptest.NewRecorder()
	req := newRequest("PUT", "/api/issues/"+issueID+"/metadata/gate_review", json.RawMessage(`{"value":"pending@abc1234"}`))
	req = withURLParams(req, "id", issueID, "key", "gate_review")
	testHandler.SetIssueMetadataKey(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Set gate_review: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req = newRequest("PUT", "/api/issues/"+issueID+"/metadata/pipeline_status", json.RawMessage(`{"value":"waiting_review"}`))
	req = withURLParams(req, "id", issueID, "key", "pipeline_status")
	testHandler.SetIssueMetadataKey(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Set pipeline_status: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if got := countGateTasksForAgent(t, ctx, issueID, reviewerID); got != 1 {
		t.Fatalf("existing active reviewer run should be reused, got %d tasks", got)
	}
	metadata := loadIssueMetadata(t, ctx, issueID)
	if got := metadata["gate_dispatch_status"]; got != "agent_run_active" {
		t.Fatalf("gate_dispatch_status = %v, want agent_run_active", got)
	}
	if got := metadata["gate_dispatch_head"]; got != "abc1234" {
		t.Fatalf("gate_dispatch_head = %v, want abc1234", got)
	}
}

func createMetadataTestIssue(t *testing.T, title string) string {
	t.Helper()
	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":    title,
		"status":   "todo",
		"priority": "medium",
	})
	testHandler.CreateIssue(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("createMetadataTestIssue: %d %s", w.Code, w.Body.String())
	}
	var issue IssueResponse
	if err := json.NewDecoder(w.Body).Decode(&issue); err != nil {
		t.Fatalf("decode issue: %v", err)
	}
	return issue.ID
}

func countGateTasksForAgent(t *testing.T, ctx context.Context, issueID, agentID string) int {
	t.Helper()

	var taskCount int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
	`, issueID, agentID).Scan(&taskCount); err != nil {
		t.Fatalf("count gate tasks: %v", err)
	}
	return taskCount
}

func loadIssueMetadata(t *testing.T, ctx context.Context, issueID string) map[string]any {
	t.Helper()

	var metadataRaw []byte
	if err := testPool.QueryRow(ctx, `SELECT metadata FROM issue WHERE id = $1`, issueID).Scan(&metadataRaw); err != nil {
		t.Fatalf("load issue metadata: %v", err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(metadataRaw, &metadata); err != nil {
		t.Fatalf("decode issue metadata: %v", err)
	}
	return metadata
}

func createNamedHandlerTestAgent(t *testing.T, name string) string {
	t.Helper()

	var agentID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO agent (
			workspace_id, name, description, runtime_mode, runtime_config,
			runtime_id, visibility, max_concurrent_tasks, owner_id,
			instructions, custom_env, custom_args
		)
		VALUES ($1, $2, '', 'cloud', '{}'::jsonb, $3, 'workspace', 1, $4, '', '{}'::jsonb, '[]'::jsonb)
		RETURNING id
	`, testWorkspaceID, name, handlerTestRuntimeID(t), testUserID).Scan(&agentID); err != nil {
		t.Fatalf("create named handler test agent: %v", err)
	}

	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE agent_id = $1`, agentID)
		testPool.Exec(context.Background(), `DELETE FROM agent WHERE id = $1`, agentID)
	})

	return agentID
}
