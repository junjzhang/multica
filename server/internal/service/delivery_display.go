package service

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// deliveryTitleLimit bounds the title copied into a prompt. A delivery prompt
// is read by a person at a glance; the exact title stays readable on the issue.
const deliveryTitleLimit = 200

// DeliveryDisplay is the reader-facing context of one issue delivery. It is
// presentation only: every delivery keeps working from its stable IDs and
// commands when this is empty.
type DeliveryDisplay struct {
	Identifier string
	Title      string
	URL        string
}

// ResolveDeliveryDisplay builds the identifier, short title, and browser link
// for a delivery prompt. An unresolvable workspace or a deployment without a
// public app URL degrades to the fields that are known instead of failing the
// delivery.
func ResolveDeliveryDisplay(ctx context.Context, q *db.Queries, issue db.Issue, appURL string) DeliveryDisplay {
	display := DeliveryDisplay{Title: truncateDeliveryTitle(issue.Title)}
	workspace, err := q.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return display
	}
	if prefix := strings.TrimSpace(workspace.IssuePrefix); prefix != "" {
		display.Identifier = fmt.Sprintf("%s-%d", prefix, issue.Number)
	}
	display.URL = IssueBrowserURL(appURL, workspace.Slug, display.Identifier)
	return display
}

// IssueBrowserURL joins the configured app origin with the workspace-scoped
// issue route. It returns "" rather than a partial link so a reader never sees
// a URL that does not resolve.
func IssueBrowserURL(appURL, workspaceSlug, identifier string) string {
	origin := strings.TrimRight(strings.TrimSpace(appURL), "/")
	slug := strings.TrimSpace(workspaceSlug)
	key := strings.TrimSpace(identifier)
	if origin == "" || slug == "" || key == "" {
		return ""
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return origin + "/" + url.PathEscape(slug) + "/issues/" + url.PathEscape(key)
}

// truncateDeliveryTitle keeps the prompt line bounded without splitting a
// UTF-8 sequence or leaving control characters in the delivered text.
func truncateDeliveryTitle(title string) string {
	trimmed := strings.TrimSpace(sanitizeDeliveryText(title))
	runes := []rune(trimmed)
	if len(runes) <= deliveryTitleLimit {
		return trimmed
	}
	return string(runes[:deliveryTitleLimit]) + "…"
}

// sanitizeDeliveryText drops control characters: the prompt is assembled from
// stored user text, and a raw newline would break the key/value envelope the
// plugin parses.
func sanitizeDeliveryText(text string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, text)
}
