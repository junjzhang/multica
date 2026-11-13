package main

import (
	"context"
	"math/rand/v2"
	"testing"
)

func TestTGitSnapshotMigrationsPreserveExistingMRs(t *testing.T) {
	ctx := context.Background()
	schema := createScratchSchema(t, ctx, openTestPool(t), "migrate_tgit_snapshot_")
	pool := openTestPoolWithSearchPath(t, schema)
	if _, err := pool.Exec(ctx, `CREATE TABLE vcs_pull_request(id uuid PRIMARY KEY DEFAULT gen_random_uuid(),provider text,state text); INSERT INTO vcs_pull_request(provider,state) VALUES('tgit','open'),('gitlab','merged')`); err != nil {
		t.Fatal(err)
	}
	versions := []string{"609_vcs_api_snapshot", "610_tgit_snapshot_refresh_index"}
	opts := runOptions{Direction: "up", Files: realMigrationFiles(t, versions, "up"), SchemaMigrationsTable: schema + ".schema_migrations", AdvisoryLockKey: int64(rand.Uint64()&0x7fffffffffffffff) | 1}
	if err := runMigrations(ctx, pool, opts); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM vcs_pull_request WHERE api_snapshot='{}'::jsonb AND snapshot_fetched_at IS NULL AND snapshot_refresh_after IS NULL AND NOT snapshot_error`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("old MRs received fabricated snapshots: %d %v", count, err)
	}
	var valid bool
	if err := pool.QueryRow(ctx, `SELECT indisvalid FROM pg_index WHERE indexrelid=$1::regclass`, schema+".idx_tgit_snapshot_refresh").Scan(&valid); err != nil || !valid {
		t.Fatalf("refresh index unavailable: %v", err)
	}
	opts.Direction = "down"
	opts.Files = realMigrationFiles(t, []string{"610_tgit_snapshot_refresh_index", "609_vcs_api_snapshot"}, "down")
	if err := runMigrations(ctx, pool, opts); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM vcs_pull_request`).Scan(&count); err != nil || count != 2 {
		t.Fatal("rollback lost existing MRs")
	}
	opts.Direction = "up"
	opts.Files = realMigrationFiles(t, versions, "up")
	if err := runMigrations(ctx, pool, opts); err != nil {
		t.Fatal(err)
	}
}
