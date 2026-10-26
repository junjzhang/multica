package vcs

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type tgitProvider struct{}

func init() { register(tgitProvider{}) }

func (tgitProvider) Kind() Kind { return KindTGit }

func (tgitProvider) EventKind(h http.Header) EventKind {
	if h.Get("X-Event") == "Merge Request Hook" {
		return EventPullRequest
	}
	return EventOther
}

func (tgitProvider) VerifySignature(secret string, h http.Header, _ []byte) bool {
	return secret != "" && subtle.ConstantTimeCompare([]byte(h.Get("X-Token")), []byte(secret)) == 1
}

func (tgitProvider) ParsePullRequest(body []byte) (PullRequestEvent, error) {
	var payload struct {
		ObjectKind       string `json:"object_kind"`
		ObjectAttributes struct {
			IID            int32  `json:"iid"`
			Title          string `json:"title"`
			Description    string `json:"description"`
			State          string `json:"state"`
			Action         string `json:"action"`
			SourceBranch   string `json:"source_branch"`
			URL            string `json:"url"`
			WorkInProgress bool   `json:"work_in_progress"`
			CreatedAt      string `json:"created_at"`
			UpdatedAt      string `json:"updated_at"`
			Target         struct {
				WebURL string `json:"web_url"`
			} `json:"target"`
			LastCommit struct {
				ID string `json:"id"`
			} `json:"last_commit"`
		} `json:"object_attributes"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return PullRequestEvent{}, err
	}
	a := payload.ObjectAttributes
	// TGit's setup probes have no source commit and must not replace real MRs.
	if payload.ObjectKind != "merge_request" || a.IID <= 0 || strings.TrimSpace(a.LastCommit.ID) == "" {
		return PullRequestEvent{}, errors.New("tgit: invalid merge request")
	}
	target, err := url.Parse(a.Target.WebURL)
	if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		return PullRequestEvent{}, errors.New("tgit: invalid target repository URL")
	}
	owner, name := splitNamespace(target.Path)
	if owner == "" || name == "" {
		return PullRequestEvent{}, errors.New("tgit: missing target repository identity")
	}
	link, err := url.Parse(a.URL)
	if err != nil || link.Host != target.Host || (link.Scheme != "http" && link.Scheme != "https") || link.User != nil {
		return PullRequestEvent{}, errors.New("tgit: invalid merge request URL")
	}
	expectedPath := strings.TrimRight(target.Path, "/") + "/merge_requests/" + fmt.Sprint(a.IID)
	if strings.Replace(link.Path, "/-/merge_requests/", "/merge_requests/", 1) != expectedPath {
		return PullRequestEvent{}, errors.New("tgit: merge request URL does not match target repository")
	}
	state := a.State
	switch state {
	case "opened", "reopened":
		state = "open"
		if a.WorkInProgress || strings.HasPrefix(strings.ToLower(a.Title), "wip:") || strings.HasPrefix(strings.ToLower(a.Title), "draft:") {
			state = "draft"
		}
	case "closed", "merged":
	default:
		return PullRequestEvent{}, fmt.Errorf("tgit: unsupported MR state %q", state)
	}
	created, err := parseTGitTime(a.CreatedAt)
	if err != nil {
		return PullRequestEvent{}, err
	}
	updated, err := parseTGitTime(a.UpdatedAt)
	if err != nil {
		return PullRequestEvent{}, err
	}
	// The payload identifies the event actor and update time, not the MR author or merge/close time.
	return PullRequestEvent{
		Action: a.Action, RepoOwner: owner, RepoName: name, Number: a.IID,
		Title: a.Title, Body: a.Description, State: state, HTMLURL: a.URL,
		Branch: a.SourceBranch, HeadSHA: a.LastCommit.ID,
		CreatedAt: created, UpdatedAt: updated,
	}, nil
}

func parseTGitTime(raw string) (string, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999-0700"} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.UTC().Format(time.RFC3339Nano), nil
		}
	}
	return "", errors.New("tgit: missing or invalid event timestamp")
}

func (tgitProvider) ParseCIStatus(_ []byte) (CIStatusEvent, error) {
	return CIStatusEvent{}, errors.New("tgit: CI status webhooks are not supported")
}

func (tgitProvider) ValidateToken(ctx context.Context, instanceURL, token string) (Account, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, NormalizeInstanceURL(instanceURL)+"/api/v3/user", nil)
	if err != nil {
		return Account{}, fmt.Errorf("tgit: build request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", token)
	req.Header.Set("Accept", "application/json")
	// Credentials must not follow an instance redirect to another host.
	client := *httpClient
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return Account{}, fmt.Errorf("tgit: request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return Account{}, ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return Account{}, fmt.Errorf("tgit: GET /user: status %d", resp.StatusCode)
	}
	var user struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return Account{}, fmt.Errorf("tgit: decode user: %w", err)
	}
	if user.Username == "" {
		return Account{}, errors.New("tgit: user response missing username")
	}
	return Account{Login: user.Username}, nil
}
