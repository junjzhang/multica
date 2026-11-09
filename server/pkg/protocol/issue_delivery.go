package protocol

// ExternalIssueRequest carries the same squad context as a managed claim.
// The mailbox ID, rather than a fabricated task ID, proves an external turn.
type ExternalIssueRequest struct {
	Schema    string `json:"schema"`
	IssueID   string `json:"issue_id"`
	CommentID string `json:"comment_id,omitempty"`
	SquadID   string `json:"squad_id,omitempty"`
	Briefing  string `json:"briefing,omitempty"`
}
