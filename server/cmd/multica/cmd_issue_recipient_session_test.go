package main

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestIssueWritesCarryExplicitRecipientSession(t *testing.T) {
	for _, operation := range []string{"create", "update", "assign"} {
		t.Run(operation, func(t *testing.T) {
			var body map[string]any
			server := coordinationTestServer(t, &body)
			defer server.Close()
			setCoordinationTestEnv(t, server.URL)
			var command *cobra.Command
			switch operation {
			case "create":
				command = newIssueCreateTestCmd()
				_ = command.Flags().Set("title", "Explicit external recipient")
			case "update":
				command = newIssueUpdateTestCmd()
			case "assign":
				command = newIssueAssignTestCmd()
				_ = command.Flags().Set("to-id", testCoordinatorMemberID)
			}
			command.Flags().String("recipient-session-id", "", "")
			if err := command.Flags().Set("recipient-session-id", "33333333-3333-4333-8333-333333333333"); err != nil {
				t.Fatal(err)
			}
			var err error
			switch operation {
			case "create":
				err = runIssueCreate(command, nil)
			case "update":
				err = runIssueUpdate(command, []string{testCoordinatorIssueID})
			case "assign":
				err = runIssueAssign(command, []string{testCoordinatorIssueID})
			}
			if err != nil {
				t.Fatal(err)
			}
			if body["recipient_session_id"] != "33333333-3333-4333-8333-333333333333" {
				t.Fatalf("recipient selection lost: %v", body)
			}
		})
	}
}
