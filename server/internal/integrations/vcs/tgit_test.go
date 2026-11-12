package vcs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const tgitMR = `{
  "object_kind":"merge_request",
  "user":{"username":"merging-reviewer"},
  "object_attributes":{
    "iid":42,"title":"WIP: Fix WORK-1","description":"Related WORK-1",
    "state":"opened","action":"open","source_branch":"feature/fix",
    "created_at":"2026-10-26T06:39:29+0000","updated_at":"2026-10-26T07:38:06.123+0000",
    "source":{"web_url":"https://git.test/contributor/fork"},
    "target":{"name":"widget","namespace":"org/sub/widget","web_url":"https://git.test/org/sub/widget"},
    "url":"https://git.test/org/sub/widget/merge_requests/42",
    "last_commit":{"id":"head-42","author":{"name":"commit-author"}}
  }
}`

func TestTGitMRContract(t *testing.T) {
	p, ok := For("tgit")
	if !ok || !p.Kind().Valid() {
		t.Fatal("TGit provider unavailable")
	}
	pr, err := p.ParsePullRequest([]byte(tgitMR))
	if err != nil {
		t.Fatal(err)
	}
	if pr.RepoOwner != "org/sub" || pr.RepoName != "widget" || pr.Number != 42 || pr.HeadSHA != "head-42" {
		t.Fatalf("fork MR must be keyed by target repository: %+v", pr)
	}
	if pr.State != "draft" || pr.AuthorLogin != "" || pr.CreatedAt != "2026-10-26T06:39:29Z" || pr.UpdatedAt != "2026-10-26T07:38:06.123Z" {
		t.Fatalf("wrong state, author or event clock: %+v", pr)
	}
	for _, state := range []string{"closed", "merged"} {
		t.Run(state, func(t *testing.T) {
			pr, err := p.ParsePullRequest([]byte(strings.Replace(tgitMR, `"state":"opened"`, `"state":"`+state+`"`, 1)))
			if err != nil || pr.State != state {
				t.Fatalf("terminal state must override WIP title: %+v, %v", pr, err)
			}
		})
	}
	for _, corrupt := range []string{
		strings.Replace(tgitMR, "2026-10-26T07:38:06.123+0000", "invalid", 1),
		strings.Replace(tgitMR, `"state":"opened"`, `"state":"unknown"`, 1),
		strings.Replace(tgitMR, `"iid":42`, `"iid":0`, 1),
		strings.Replace(tgitMR, "https://git.test/org/sub/widget/merge_requests/42", "javascript:alert(1)", 1),
		strings.Replace(tgitMR, "https://git.test/org/sub/widget/merge_requests/42", "https://other.test/org/sub/widget/merge_requests/42", 1),
		strings.Replace(tgitMR, "https://git.test/org/sub/widget/merge_requests/42", "https://git.test/org/sub/other/merge_requests/42", 1),
	} {
		if _, err := p.ParsePullRequest([]byte(corrupt)); err == nil {
			t.Fatal("malformed event must not overwrite a valid stored MR")
		}
	}
}

func TestTGitWebhookAuthentication(t *testing.T) {
	p := tgitProvider{}
	h := http.Header{}
	h.Set("X-Event", "Merge Request Hook")
	h.Set("X-Token", "secret")
	if p.EventKind(h) != EventPullRequest || !p.VerifySignature("secret", h, nil) {
		t.Fatal("documented TGit event and token rejected")
	}
	if p.VerifySignature("", h, nil) || p.VerifySignature("other", h, nil) {
		t.Fatal("invalid secret accepted")
	}
	h.Del("X-Token")
	h.Set("X-Gitlab-Token", "secret")
	if p.VerifySignature("secret", h, nil) {
		t.Fatal("a different provider's auth header must not authenticate TGit")
	}
	h.Set("X-Event", "Pipeline Hook")
	if p.EventKind(h) != EventOther {
		t.Fatal("unsupported CI event must not publish a result")
	}
}

func TestTGitTokenValidation(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v3/user" || r.Header.Get("PRIVATE-TOKEN") != "tok" {
				t.Errorf("wrong TGit authentication request: %s", r.URL.Path)
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"username":"alice"}`))
		}))
		account, err := (tgitProvider{}).ValidateToken(context.Background(), srv.URL, "tok")
		srv.Close()
		if status == http.StatusOK && (err != nil || account.Login != "alice") {
			t.Fatalf("account=%+v error=%v", account, err)
		}
		if (status == http.StatusUnauthorized || status == http.StatusForbidden) && !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("status=%d error=%v", status, err)
		}
		if status == http.StatusInternalServerError && err == nil {
			t.Fatal("server error accepted")
		}
	}
}

func TestTGitTokenDoesNotFollowRedirect(t *testing.T) {
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("token validation followed a redirect")
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	defer origin.Close()
	if _, err := (tgitProvider{}).ValidateToken(context.Background(), origin.URL, "private"); err == nil {
		t.Fatal("redirect accepted as token validation")
	}
}
