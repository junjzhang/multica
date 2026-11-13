package migrations

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestManagedReturnContinuationRollbackPreservesProofs(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("integration test requires Postgres at DATABASE_URL")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, kind := range []string{"unused", "continued", "forwarded"} {
		t.Run(kind, func(t *testing.T) {
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `
CREATE TEMP TABLE issue_delegation_return (id uuid) ON COMMIT DROP;
CREATE TEMP TABLE agent_task_delegation_return (delegation_return_id uuid) ON COMMIT DROP;
`+readMigrationFile(t, "662_managed_return_continuation.up.sql")); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "continued":
				_, err = tx.Exec(ctx, `INSERT INTO issue_delegation_return VALUES (gen_random_uuid(), gen_random_uuid())`)
			case "forwarded":
				_, err = tx.Exec(ctx, `INSERT INTO agent_task_delegation_return VALUES (gen_random_uuid(), gen_random_uuid())`)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `SAVEPOINT before_down`); err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(ctx, readMigrationFile(t, "662_managed_return_continuation.down.sql"))
			if kind == "unused" {
				if err != nil {
					t.Fatalf("unused schema rollback: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "cannot roll back managed return continuation") {
				t.Fatalf("rollback erased %s proofs: %v", kind, err)
			}
			if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT before_down`); err != nil {
				t.Fatal(err)
			}
			var retained int
			if err := tx.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM issue_delegation_return WHERE continued_from_return_id IS NOT NULL) +
 (SELECT count(*) FROM agent_task_delegation_return WHERE delivery_return_id <> delegation_return_id)`).Scan(&retained); err != nil || retained != 1 {
				t.Fatalf("proofs after refused rollback = %d: %v", retained, err)
			}
		})
	}
}
