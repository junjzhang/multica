package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestIssueHandoffNoStart(t *testing.T) {
	const issueID = "11111111-1111-4111-8111-111111111111"
	for _, command := range []struct {
		group, endpoint string
	}{
		{"executor", "/executor/handoff"},
		{"leadership", "/task-leader/handoff"},
	} {
		t.Run(command.group, func(t *testing.T) {
			handoff, _, err := issueCmd.Find([]string{command.group, "handoff"})
			if err != nil || handoff.Name() != "handoff" {
				t.Fatalf("find handoff command: %v", err)
			}
			for _, scenario := range []struct {
				name, file, want string
				noStart          bool
			}{
				{"default request", `{"reason":"take ownership"}`, "", false},
				{"suppress by flag", `{"reason":"take ownership"}`, "true", true},
				{"request file wins", `{"reason":"take ownership","suppress_run":false}`, "false", true},
				{"suppress by request file", `{"reason":"take ownership","suppress_run":true}`, "true", false},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					t.Chdir(t.TempDir())
					if err := os.WriteFile("request.json", []byte(scenario.file), 0o600); err != nil {
						t.Fatal(err)
					}
					var payload map[string]json.RawMessage
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method != http.MethodPost || r.URL.Path != "/api/issues/"+issueID+command.endpoint {
							t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
						}
						if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
							t.Errorf("decode request: %v", err)
						}
						_, _ = w.Write([]byte(`{"issue_revision":2}`))
					}))
					defer server.Close()
					setCLITestServerEnv(t, server.URL)
					t.Setenv("MULTICA_TOKEN", "mat_test-token")
					flags := []string{"--request-file", "request.json"}
					if scenario.noStart {
						flags = append(flags, "--no-start")
					}
					for _, name := range []string{"request-file", "no-start"} {
						flag := handoff.Flags().Lookup(name)
						if flag == nil {
							t.Fatalf("missing --%s", name)
						}
						value, changed := flag.Value.String(), flag.Changed
						t.Cleanup(func() {
							_ = flag.Value.Set(value)
							flag.Changed = changed
						})
					}
					if err := handoff.ParseFlags(flags); err != nil {
						t.Fatal(err)
					}
					if err := handoff.RunE(handoff, []string{issueID}); err != nil {
						t.Fatal(err)
					}
					if string(payload["suppress_run"]) != scenario.want || string(payload["reason"]) != `"take ownership"` {
						t.Fatalf("unexpected request body: %v", payload)
					}
				})
			}
		})
	}
}
