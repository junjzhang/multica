package handler

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func (h *Handler) hydrateAcceptedReturns(ctx context.Context, workspace pgtype.UUID, tasks []AgentTaskResponse) error {
	ids := make([]pgtype.UUID, 0, len(tasks))
	byID := make(map[string]*AgentTaskResponse, len(tasks))
	for i := range tasks {
		ids = append(ids, parseUUID(tasks[i].ID))
		byID[tasks[i].ID] = &tasks[i]
	}
	rows, err := h.Queries.ListAcceptedTaskReturns(ctx, db.ListAcceptedTaskReturnsParams{WorkspaceID: workspace, TaskIds: ids})
	if err != nil {
		return err
	}
	for i := range tasks {
		empty := []protocol.AcceptedReturn{}
		tasks[i].AcceptedReturns = &empty
	}
	for _, row := range rows {
		task := byID[uuidToString(row.TaskID)]
		*task.AcceptedReturns = append(*task.AcceptedReturns, protocol.AcceptedReturn{
			ReturnID: uuidToString(row.ReturnID), RecipientAgentID: uuidToString(row.DispatcherAgentID),
			Kind: row.ReturnKind, SourceTaskID: uuidToString(row.SourceTaskID), ExternalSessionID: uuidToString(row.ExternalSessionID),
		})
	}
	return nil
}
