package main

import (
	"context"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestIssueContractMigrationsUpDownUpInIsolatedSchema(t *testing.T) {
	base := openTestPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	schema := createScratchSchema(t, ctx, base, "issue_contract_")
	pool := openTestPoolWithSearchPath(t, schema)
	_, err := pool.Exec(ctx, `CREATE TABLE issue (
		id uuid DEFAULT gen_random_uuid(),workspace_id uuid,status text,
		assignee_type text,assignee_id uuid,coordinator_type text,coordinator_id uuid,
		merge_allowed boolean DEFAULT false,merge_repository_url text,merge_target_branch text,
		merge_granted_by uuid,merge_granted_at timestamptz,acceptance_criteria jsonb DEFAULT '[]');
		CREATE FUNCTION issue_effective_status(uuid,text) RETURNS text LANGUAGE sql IMMUTABLE AS 'SELECT $2';`)
	if err != nil {
		t.Fatal(err)
	}
	fx := testutil.New(pool, "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002")
	for _, direction := range []string{"up", "down", "up"} {
		versions := []string{"650_issue_contract", "651_issue_contract_unique", "652_issue_contract_completion", "653_contract_permission_maintenance"}
		if direction == "down" {
			slices.Reverse(versions)
		}
		if err := runMigrations(ctx, pool, runOptions{Direction: direction, Files: realMigrationFiles(t, versions, direction), SchemaMigrationsTable: schema + ".schema_migrations", AdvisoryLockKey: int64(rand.Uint64()&0x7fffffffffffffff) | 1, Hooks: hooksForDirection(direction), Conditions: conditionsForDirection(direction)}); err != nil {
			t.Fatalf("contract migrations %s: %v", direction, err)
		}
		var exists bool
		if err := pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", schema+".issue_contract").Scan(&exists); err != nil || exists != (direction == "up") {
			t.Fatalf("table state after %s: %v %v", direction, exists, err)
		}
		if direction == "down" {
			continue
		}
		var valid bool
		if err := pool.QueryRow(ctx, "SELECT indisvalid AND indisunique FROM pg_index WHERE indexrelid=to_regclass($1)", schema+".issue_contract_issue_unique").Scan(&valid); err != nil || !valid {
			t.Fatalf("contract index invalid: %v", err)
		}
		issue := fx.Insert(t, "issue", testutil.Cols{"workspace_id": fx.WorkspaceID, "status": "in_progress", "acceptance_criteria": []byte(`["verified"]`), "merge_allowed": true})
		fx.InsertNoID(t, "issue_contract", testutil.Cols{"issue_id": issue, "workspace_id": fx.WorkspaceID, "data": testutil.Raw("jsonb_build_object('versions',jsonb_build_array(jsonb_build_object('number',1,'criteria',jsonb_build_array('verified'),'basis',(SELECT issue_contract_basis(i) FROM issue i WHERE i.id='" + issue + "'::uuid))),'decisions','[]'::jsonb)")}, "issue_id=$1", issue)
		if _, err := pool.Exec(ctx, "UPDATE issue SET status='done' WHERE id=$1", issue); err == nil {
			t.Fatal("unaccepted contract entered done")
		}
		fx.Exec(t, `UPDATE issue_contract SET data=jsonb_set(data,'{decisions}','[{"outcome":"accept","version":1,"criteria":[0]}]'::jsonb) WHERE issue_id=$1`, issue)
		fx.Exec(t, "UPDATE issue SET status='done' WHERE id=$1", issue)
		fx.Exec(t, "UPDATE issue SET merge_allowed=false WHERE id=$1", issue)
		var allowed bool
		fx.QueryRow(t, "SELECT merge_allowed FROM issue WHERE id=$1", issue).Scan(&allowed)
		if allowed {
			t.Fatal("completed-issue permission revocation failed")
		}
	}
}
