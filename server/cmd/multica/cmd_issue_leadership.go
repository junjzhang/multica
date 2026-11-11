package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

func init() {
	leadership := &cobra.Command{Use: "leadership", Short: "Claim task leadership or explicitly hand it off"}
	for _, action := range []string{"claim", "handoff"} {
		command := &cobra.Command{Use: action + " <issue-id>", Short: "Apply a leadership request from a UTF-8 JSON file", Args: exactArgs(1), RunE: runIssueLeadership}
		command.Flags().String("request-file", "", "JSON request including expected_revision and expected_issue_revision")
		command.Flags().String("output", "json", "Output format: json")
		_ = command.MarkFlagRequired("request-file")
		leadership.AddCommand(command)
	}
	issueCmd.AddCommand(leadership)
	executor := &cobra.Command{Use: "executor", Short: "Manage the task executor"}
	handoff := &cobra.Command{Use: "handoff <issue-id>", Short: "Hand off an external executor from a UTF-8 JSON file", Args: exactArgs(1), RunE: runIssueLeadership}
	handoff.Flags().String("request-file", "", "JSON request including expected_issue_revision")
	handoff.Flags().String("output", "json", "Output format: json")
	_ = handoff.MarkFlagRequired("request-file")
	executor.AddCommand(handoff)
	issueCmd.AddCommand(executor)
}

func runIssueLeadership(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	path, _ := cmd.Flags().GetString("request-file")
	if err = ensureFileFlagWithinWorkdir(cmd, "request-file", "request", path); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !json.Valid(data) {
		return fmt.Errorf("request-file must contain valid JSON")
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	ref, err := resolveIssueRef(ctx, client, args[0])
	if err != nil {
		return err
	}
	var result json.RawMessage
	endpoint := "/leadership/claim"
	if cmd.Name() == "handoff" {
		endpoint = "/task-leader/handoff"
		if cmd.Parent().Name() == "executor" {
			endpoint = "/executor/handoff"
		}
	}
	if err = client.PostJSON(ctx, "/api/issues/"+ref.ID+endpoint, json.RawMessage(data), &result); err != nil {
		return err
	}
	return cli.PrintJSON(os.Stdout, result)
}
