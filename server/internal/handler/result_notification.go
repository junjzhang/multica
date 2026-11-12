package handler

import (
	"context"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type ResultNotificationResponse struct {
	ReturnID         string `json:"return_id"`
	CurrentReturnID  string `json:"current_return_id,omitempty"`
	TakeoverID       string `json:"takeover_id,omitempty"`
	RecipientAgentID string `json:"recipient_agent_id"`
	Kind             string `json:"kind"`
	DeliveryID       string `json:"delivery_id,omitempty"`
	HandlingTaskID   string `json:"handling_task_id,omitempty"`
	Status           string `json:"status"`
	Error            string `json:"error,omitempty"`
}

func (h *Handler) hydrateResultNotifications(ctx context.Context, workspace, issue pgtype.UUID, tasks []AgentTaskResponse) error {
	rows, err := h.Queries.ListIssueResultNotifications(ctx, db.ListIssueResultNotificationsParams{WorkspaceID: workspace, IssueID: issue})
	if err != nil {
		return err
	}
	byID := make(map[string]*AgentTaskResponse, len(tasks))
	for i := range tasks {
		byID[tasks[i].ID] = &tasks[i]
		empty := []ResultNotificationResponse{}
		tasks[i].ResultNotifications = &empty
	}
	for _, row := range rows {
		task := byID[uuidToString(row.TaskID)]
		if task == nil {
			continue
		}
		note := ResultNotificationResponse{CurrentReturnID: uuidToString(row.CurrentReturnID), TakeoverID: uuidToString(row.TakeoverID), ReturnID: uuidToString(row.DelegationReturnID), RecipientAgentID: uuidToString(row.DispatcherAgentID), Kind: row.ReturnKind, DeliveryID: uuidToString(row.NotificationDeliveryID), HandlingTaskID: uuidToString(row.NotificationTaskID), Status: "pending"}
		switch {
		case row.NotificationError.Valid:
			note.Status, note.Error = "refused", row.NotificationError.String
		case row.NotificationDeliveryID.Valid && !row.NotificationTaskID.Valid:
			note.Status, note.Error = row.DeliveryStatus.String, row.DeliveryError.String
			if note.Status == "" {
				note.Status = "unknown"
			}
		case row.NotificationTaskID.Valid:
			note.Status, note.Error = row.HandlingStatus.String, row.HandlingError.String
			switch row.HandlingStatus.String {
			case "completed":
				note.Status = "received"
			case "failed":
				note.Status = "unknown"
				if row.HandlingFailureReason.String == protocol.DelegationResultUnavailable {
					note.Status = "refused"
				}
			case "cancelled":
				if row.HandlingStartedAt.Valid {
					note.Status = "unknown"
				}
			case "":
				note.Status = "unknown"
			}
		}
		*task.ResultNotifications = append(*task.ResultNotifications, note)
	}
	return nil
}
