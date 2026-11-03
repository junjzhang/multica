export type ResultTakeoverTarget =
  | { kind: "external_session"; external_session_id: string }
  | { kind: "managed_task"; source_task_id: string; selection_version: string };

export interface ResultTakeoverRequest {
  client_request_id: string;
  result_task_id: string;
  from_return_id: string;
  expected_current_return_id: string;
  target: ResultTakeoverTarget;
}

export interface ResultTakeoverResponse {
  id: string;
  client_request_id: string;
  result_task_id: string;
  from_return_id: string;
  /** Immutable destination of this action, including on replay. */
  target_return_id: string;
  /** The route now in effect; a later action may have changed it. */
  current_return_id: string;
  created_at: string;
}

export interface ResultReturnTargetDisplay {
  provider: string;
  session_ref: string;
  display_name: string;
}

export interface ResultReturnVersion {
  return_id: string;
  kind: "managed_task" | "external_session";
  /** Null means unavailable or unauthorized detail, never an absent return. */
  target: ResultReturnTargetDisplay | null;
}

export type ResultTakeoverChoice = ResultTakeoverTarget & ResultReturnTargetDisplay & { available: boolean };

export interface ResultTakeoverOptions {
  current_return_id: string;
  returns: ResultReturnVersion[];
  targets: ResultTakeoverChoice[];
}
