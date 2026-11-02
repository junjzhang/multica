package main

import (
	"context"
	"math/rand/v2"
	"testing"
)

func TestTGitProviderMigrationRollbackPreservesRows(t *testing.T) {
	ctx := context.Background()
	schema := createScratchSchema(t, ctx, openTestPool(t), "migrate_tgit_")
	pool := openTestPoolWithSearchPath(t, schema)
	if _, err := pool.Exec(ctx, `
		CREATE TABLE vcs_connection (provider TEXT CHECK (provider IN ('forgejo','gitea','gitlab')));
		CREATE TABLE vcs_pull_request (provider TEXT CHECK (provider IN ('forgejo','gitea','gitlab')));
		INSERT INTO vcs_connection VALUES ('gitlab');
		INSERT INTO vcs_pull_request VALUES ('gitlab');
	`); err != nil {
		t.Fatal(err)
	}
	const version = "608_tgit_provider"
	opts := runOptions{
		Direction: "up", Files: realMigrationFiles(t, []string{version}, "up"),
		SchemaMigrationsTable: schema + ".schema_migrations",
		AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
	}
	if err := runMigrations(ctx, pool, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO vcs_connection VALUES ('tgit'); INSERT INTO vcs_pull_request VALUES ('tgit')`); err != nil {
		t.Fatal(err)
	}
	opts.Direction, opts.Files = "down", realMigrationFiles(t, []string{version}, "down")
	if err := runMigrations(ctx, pool, opts); err == nil {
		t.Fatal("rollback must refuse while TGit rows remain")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM vcs_connection WHERE provider='tgit'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("failed rollback lost connection data: count=%d err=%v", count, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO vcs_pull_request VALUES ('unknown')`); err == nil {
		t.Fatal("failed rollback removed the provider constraint")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM vcs_connection WHERE provider='tgit'; DELETE FROM vcs_pull_request WHERE provider='tgit'`); err != nil {
		t.Fatal(err)
	}
	if err := runMigrations(ctx, pool, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO vcs_connection VALUES ('tgit')`); err == nil {
		t.Fatal("rollback did not restore the old constraint")
	}
	opts.Direction, opts.Files = "up", realMigrationFiles(t, []string{version}, "up")
	if err := runMigrations(ctx, pool, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO vcs_connection VALUES ('tgit')`); err != nil {
		t.Fatal(err)
	}
}
