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
	contract := &cobra.Command{Use: "contract", Short: "Read agreements and submit or decide delivery"}
	get := &cobra.Command{Use: "get <issue-id>", Short: "Read the current agreement and delivery history", Args: exactArgs(1), RunE: runIssueContract}
	apply := &cobra.Command{Use: "apply <issue-id>", Short: "Apply a versioned contract request from a UTF-8 JSON file", Args: exactArgs(1), RunE: runIssueContract}
	apply.Flags().String("request-file", "", "JSON request with action, expected_sequence and client_request_id")
	apply.MarkFlagRequired("request-file")
	for _, c := range []*cobra.Command{get, apply} {
		c.Flags().String("output", "json", "Output format: json")
		contract.AddCommand(c)
	}
	issueCmd.AddCommand(contract)
}

func runIssueContract(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	ref, err := resolveIssueRef(ctx, client, args[0])
	if err != nil {
		return err
	}
	endpoint := "/api/issues/" + ref.ID + "/contract"
	var result json.RawMessage
	if cmd.Name() == "get" {
		err = client.GetJSON(ctx, endpoint, &result)
	} else {
		path, _ := cmd.Flags().GetString("request-file")
		if err := ensureFileFlagWithinWorkdir(cmd, "request-file", "request", path); err != nil {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if !json.Valid(data) {
			return fmt.Errorf("request-file must contain valid JSON")
		}
		err = client.PostJSON(ctx, endpoint, json.RawMessage(data), &result)
	}
	if err != nil {
		return err
	}
	return cli.PrintJSON(os.Stdout, result)
}
