package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	gateDispatchStatusKey = "gate_dispatch_status"
	gateDispatchHeadKey   = "gate_dispatch_head"

	gateDispatchWaiting = "waiting_for_agent_run"
	gateDispatchActive  = "agent_run_active"
	gateDispatchFailed  = "dispatch_failed"
	gateDispatchNoHead  = "waiting_for_gate_head"
)

type gateDispatchSpec struct {
	pipelineStatus string
	gateKey        string
	agentNames     []string
}

var gateDispatchSpecs = []gateDispatchSpec{
	{pipelineStatus: "waiting_review", gateKey: "gate_review", agentNames: []string{"Reviewer"}},
	{pipelineStatus: "waiting_test", gateKey: "gate_test", agentNames: []string{"QA", "QA (macOS)"}},
	{pipelineStatus: "waiting_prr", gateKey: "gate_prr", agentNames: []string{"SRE"}},
}

func gateDispatchSpecForStatus(status string) (gateDispatchSpec, bool) {
	for _, spec := range gateDispatchSpecs {
		if spec.pipelineStatus == status {
			return spec, true
		}
	}
	return gateDispatchSpec{}, false
}

func (h *Handler) reconcileGateDispatch(ctx context.Context, issue db.Issue) {
	metadata := parseIssueMetadata(issue.Metadata)
	pipelineStatus, _ := metadata["pipeline_status"].(string)
	spec, ok := gateDispatchSpecForStatus(pipelineStatus)
	if !ok {
		return
	}

	head := gateHeadFromMetadata(metadata[spec.gateKey])
	if head == "" {
		h.setIssueMetadataString(ctx, issue, gateDispatchStatusKey, gateDispatchNoHead)
		return
	}

	agent, err := h.findGateAgent(ctx, issue.WorkspaceID, spec.agentNames)
	if err != nil {
		h.markGateDispatchFailed(ctx, issue, fmt.Sprintf("%s gate dispatch failed: %v", spec.primaryAgentName(), err))
		return
	}
	if agent.ArchivedAt.Valid {
		h.markGateDispatchFailed(ctx, issue, fmt.Sprintf("%s gate dispatch failed: agent is archived", agent.Name))
		return
	}
	if !agent.RuntimeID.Valid {
		h.markGateDispatchFailed(ctx, issue, fmt.Sprintf("%s gate dispatch failed: agent has no runtime", agent.Name))
		return
	}

	if dispatchStatus, ok := h.matchingGateTaskStatus(ctx, issue.ID, agent.ID, head, metadata); ok {
		h.setGateDispatchMetadata(ctx, issue, dispatchStatus, head)
		return
	}

	if _, err := h.TaskService.EnqueueTaskForMention(ctx, issue, agent.ID, pgtype.UUID{}); err != nil {
		h.markGateDispatchFailed(ctx, issue, fmt.Sprintf("%s gate dispatch failed: %v", agent.Name, err))
		return
	}
	h.setGateDispatchMetadata(ctx, issue, gateDispatchWaiting, head)
}

func (spec gateDispatchSpec) primaryAgentName() string {
	if len(spec.agentNames) == 0 {
		return "gate agent"
	}
	return spec.agentNames[0]
}

func (h *Handler) findGateAgent(ctx context.Context, workspaceID pgtype.UUID, names []string) (db.Agent, error) {
	agents, err := h.Queries.ListAgents(ctx, workspaceID)
	if err != nil {
		return db.Agent{}, fmt.Errorf("list agents: %w", err)
	}
	for _, name := range names {
		for _, agent := range agents {
			if strings.EqualFold(strings.TrimSpace(agent.Name), name) {
				return agent, nil
			}
		}
	}
	return db.Agent{}, fmt.Errorf("agent %s not found", formatGateAgentNames(names))
}

func formatGateAgentNames(names []string) string {
	if len(names) == 0 {
		return "<none>"
	}
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, fmt.Sprintf("%q", name))
	}
	return strings.Join(quoted, " or ")
}

func (h *Handler) matchingGateTaskStatus(ctx context.Context, issueID, agentID pgtype.UUID, head string, metadata map[string]any) (string, bool) {
	tasks, err := h.Queries.ListActiveTasksByIssue(ctx, issueID)
	if err != nil {
		return "", false
	}
	for _, task := range tasks {
		if util.UUIDToString(task.AgentID) != util.UUIDToString(agentID) {
			continue
		}
		dispatchedHead, _ := metadata[gateDispatchHeadKey].(string)
		if dispatchedHead != "" && dispatchedHead != head {
			continue
		}
		switch task.Status {
		case "queued":
			return gateDispatchWaiting, true
		default:
			return gateDispatchActive, true
		}
	}
	return "", false
}

func gateHeadFromMetadata(raw any) string {
	value, ok := raw.(string)
	if !ok {
		return ""
	}
	idx := strings.LastIndex(value, "@")
	if idx < 0 || idx == len(value)-1 {
		return ""
	}
	return strings.TrimSpace(value[idx+1:])
}

func (h *Handler) markGateDispatchFailed(ctx context.Context, issue db.Issue, reason string) {
	h.setIssueMetadataString(ctx, issue, "pipeline_status", "gate_dispatch_failed")
	h.setIssueMetadataString(ctx, issue, "waiting_on", reason)
	h.setIssueMetadataString(ctx, issue, gateDispatchStatusKey, gateDispatchFailed)
}

func (h *Handler) setGateDispatchMetadata(ctx context.Context, issue db.Issue, status, head string) {
	h.setIssueMetadataString(ctx, issue, gateDispatchStatusKey, status)
	if head != "" {
		h.setIssueMetadataString(ctx, issue, gateDispatchHeadKey, head)
	}
}

func (h *Handler) setIssueMetadataString(ctx context.Context, issue db.Issue, key, value string) {
	raw, _ := json.Marshal(value)
	if _, err := h.Queries.SetIssueMetadataKey(ctx, db.SetIssueMetadataKeyParams{
		ID:          issue.ID,
		WorkspaceID: issue.WorkspaceID,
		Key:         key,
		Value:       raw,
	}); err != nil {
		slog.Warn("gate dispatch: failed to set issue metadata",
			"issue_id", util.UUIDToString(issue.ID),
			"key", key,
			"error", err,
		)
	}
}
