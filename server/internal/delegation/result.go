package delegation

import (
	"encoding/json"
	"errors"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const ResultTaskType = "delegation_result"

type ResultTaskContext struct {
	Type   string                        `json:"type"`
	Result protocol.DelegationResultTurn `json:"delegation_result"`
}

func IsResultTask(task db.AgentTaskQueue) bool {
	var value struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(task.Context, &value) == nil && value.Type == ResultTaskType
}

func ResultTurn(task db.AgentTaskQueue) (*protocol.DelegationResultTurn, error) {
	if !IsResultTask(task) {
		return nil, nil
	}
	var value ResultTaskContext
	if err := json.Unmarshal(task.Context, &value); err != nil {
		return nil, err
	}
	r := &value.Result
	if r.ResultTaskID == "" || r.DelegationReturnID == "" || r.SourceTaskID == "" ||
		r.RuntimeID == "" || r.Provider == "" || r.SessionID == "" || r.WorkDir == "" {
		return nil, errors.New("result handling task has no complete frozen address")
	}
	return r, nil
}
