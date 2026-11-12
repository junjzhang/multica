package protocol

// ExternalIssueRequest carries the same squad context as a managed claim.
// The mailbox ID, rather than a fabricated task ID, proves an external turn.
//
// The display fields are optional, presentation-only context for the delivery
// prompt. A producer that cannot resolve them (an older server, a deployment
// without a public app URL) leaves them empty, and the prompt keeps its
// ID-only shape. Nothing downstream may depend on them for identity or
// permission.
type ExternalIssueRequest struct {
	Mode            string `json:"mode,omitempty"`
	Schema          string `json:"schema"`
	IssueID         string `json:"issue_id"`
	CommentID       string `json:"comment_id,omitempty"`
	SquadID         string `json:"squad_id,omitempty"`
	Briefing        string `json:"briefing,omitempty"`
	IssueIdentifier string `json:"issue_identifier,omitempty"`
	IssueTitle      string `json:"issue_title,omitempty"`
	IssueURL        string `json:"issue_url,omitempty"`
}
