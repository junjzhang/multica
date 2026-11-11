package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const rollbackReturn = "11111111-1111-4111-8111-111111111111"
const rollbackWorkspace = "22222222-2222-4222-8222-222222222222"
const rollbackIssue = "33333333-3333-4333-8333-333333333333"
const rollbackAgent = "44444444-4444-4444-8444-444444444444"
const rollbackTask = "55555555-5555-4555-8555-555555555555"
const rollbackDelivery = "66666666-6666-4666-8666-666666666666"
const rollbackAction = "77777777-7777-4777-8777-777777777777"
const rollbackSession = "88888888-8888-4888-8888-888888888888"

func takeoverRollbackFixture(t *testing.T) (string, *pgxpool.Pool, runOptions) {
	t.Helper()
	ctx := context.Background()
	schema := createScratchSchema(t, ctx, openTestPool(t), "takeover_rollback_")
	pool := openTestPoolWithSearchPath(t, schema)
	_, err := pool.Exec(ctx, `
 CREATE TABLE issue_delegation_return (LIKE public.issue_delegation_return INCLUDING DEFAULTS INCLUDING CONSTRAINTS);
 ALTER TABLE issue_delegation_return DROP COLUMN is_current;
 CREATE TABLE agent_task_delegation_return (LIKE public.agent_task_delegation_return INCLUDING DEFAULTS INCLUDING CONSTRAINTS);
 ALTER TABLE agent_task_delegation_return DROP COLUMN takeover_id;
 CREATE TABLE agent_mailbox_message(id UUID PRIMARY KEY,target_session_id UUID,delivery_status TEXT,host_receipt TEXT);
 CREATE TABLE agent_task_queue(id UUID PRIMARY KEY,result JSONB,error TEXT);
 CREATE TABLE schema_migrations(version TEXT PRIMARY KEY,applied_at TIMESTAMPTZ NOT NULL DEFAULT now());
 INSERT INTO schema_migrations(version) VALUES('638_delegation_result_attempt_time');
 `)
	if err != nil {
		t.Fatal(err)
	}
	files := realMigrationFiles(t, []string{"619_issue_delegation_return_issue_index"}, "up")
	original, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, string(original)); err != nil {
		t.Fatal(err)
	}
	return schema, pool, runOptions{Direction: "up", SchemaMigrationsTable: schema + ".schema_migrations", AdvisoryLockKey: int64(rand.Uint64()&0x7fffffffffffffff) | 1, Hooks: hooksForDirection("up")}
}

func runTakeoverMigrations(t *testing.T, pool *pgxpool.Pool, opts runOptions, versions []string, direction string) error {
	t.Helper()
	ordered := slices.Clone(versions)
	if direction == "down" {
		slices.Reverse(ordered)
	}
	opts.Direction = direction
	opts.Files = realMigrationFiles(t, ordered, direction)
	opts.Hooks = hooksForDirection(direction)
	return runMigrations(context.Background(), pool, opts)
}

func seedOrdinaryTakeoverFacts(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO issue_delegation_return(id,workspace_id,issue_id,dispatcher_agent_id,return_kind,external_session_id) VALUES($1,$2,$3,$4,'external_session',$5)`, rollbackReturn, rollbackWorkspace, rollbackIssue, rollbackAgent, rollbackSession)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO agent_task_delegation_return(task_id,delegation_return_id,notification_delivery_id,materialized_at) VALUES($1,$2,$3,now())`, rollbackTask, rollbackReturn, rollbackDelivery)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO agent_mailbox_message VALUES($1,$2,'unknown','original-host-receipt')`, rollbackDelivery, rollbackSession)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO agent_task_queue VALUES($1,'{"output":"saved artifact"}','original-error')`, rollbackTask)
	if err != nil {
		t.Fatal(err)
	}
}

func takeoverFacts(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var data string
	err := pool.QueryRow(context.Background(), `SELECT jsonb_build_object(
 'returns',(SELECT coalesce(jsonb_agg(to_jsonb(r)-'is_current' ORDER BY id),'[]') FROM issue_delegation_return r),
 'links',(SELECT coalesce(jsonb_agg(to_jsonb(l)-'takeover_id' ORDER BY task_id,delegation_return_id),'[]') FROM agent_task_delegation_return l),
 'deliveries',(SELECT coalesce(jsonb_agg(to_jsonb(d) ORDER BY id),'[]') FROM agent_mailbox_message d),
 'results',(SELECT coalesce(jsonb_agg(to_jsonb(t) ORDER BY id),'[]') FROM agent_task_queue t))::text`).Scan(&data)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func takeoverSchemaAndHistory(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var data string
	err := pool.QueryRow(context.Background(), `SELECT jsonb_build_object(
 'returns',(SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY id),'[]') FROM issue_delegation_return r),
 'links',(SELECT coalesce(jsonb_agg(to_jsonb(l) ORDER BY task_id,delegation_return_id),'[]') FROM agent_task_delegation_return l),
 'actions',(SELECT coalesce(jsonb_agg(to_jsonb(a) ORDER BY id),'[]') FROM delegation_result_takeover a),
 'deliveries',(SELECT coalesce(jsonb_agg(to_jsonb(d) ORDER BY id),'[]') FROM agent_mailbox_message d),
 'results',(SELECT coalesce(jsonb_agg(to_jsonb(t) ORDER BY id),'[]') FROM agent_task_queue t),
 'migrations',(SELECT jsonb_agg(to_jsonb(m) ORDER BY version) FROM schema_migrations m),
 'columns',(SELECT jsonb_agg(jsonb_build_array(table_name,column_name,data_type,is_nullable) ORDER BY table_name,ordinal_position) FROM information_schema.columns WHERE table_schema=current_schema()),
 'indexes',(SELECT jsonb_agg(jsonb_build_array(c.relname,i.indisvalid,pg_get_indexdef(i.indexrelid)) ORDER BY c.relname) FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE c.relnamespace=current_schema()::regnamespace)
 )::text`).Scan(&data)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func seedTakeoverAction(t *testing.T, pool *pgxpool.Pool, withProof bool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO delegation_result_takeover(id,workspace_id,client_request_id,result_task_id,from_return_id,expected_current_return_id,target_return_id,target_kind,target_selector_id,created_by) VALUES($1,$2,$1,$3,$4,$4,$4,'external_session',$5,$6)`, rollbackAction, rollbackWorkspace, rollbackTask, rollbackReturn, rollbackSession, rollbackAgent)
	if err != nil {
		t.Fatal(err)
	}
	if withProof {
		if _, err := pool.Exec(context.Background(), `UPDATE delegation_result_takeover SET target_selection_version=repeat('a',64)`); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResultTakeoverRollbackUnusedRoundTrip(t *testing.T) {
	for _, ordinary := range []bool{false, true} {
		t.Run(fmt.Sprintf("ordinary_data_%t", ordinary), func(t *testing.T) {
			schema, pool, opts := takeoverRollbackFixture(t)
			if ordinary {
				seedOrdinaryTakeoverFacts(t, pool)
			}
			before := takeoverFacts(t, pool)
			if err := runTakeoverMigrations(t, pool, opts, resultTakeoverMigrationVersions, "up"); err != nil {
				t.Fatal(err)
			}
			if err := runTakeoverMigrations(t, pool, opts, resultTakeoverMigrationVersions, "down"); err != nil {
				t.Fatal(err)
			}
			if got := takeoverFacts(t, pool); got != before {
				t.Fatal("rollback changed ordinary result/address/receipt facts")
			}
			var ready bool
			if err := pool.QueryRow(context.Background(), `SELECT i.indisvalid AND i.indisunique AND i.indpred IS NULL FROM pg_index i WHERE i.indexrelid=to_regclass('uq_issue_delegation_return_dispatch')`).Scan(&ready); err != nil || !ready {
				t.Fatalf("legacy full unique index not restored: %v", err)
			}
			var featureExists bool
			if err := pool.QueryRow(context.Background(), `SELECT to_regclass('delegation_result_takeover') IS NOT NULL OR EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND column_name IN ('is_current','takeover_id'))`).Scan(&featureExists); err != nil || featureExists {
				t.Fatalf("feature schema remains after successful rollback: %v", err)
			}
			if ordinary {
				var inserted string
				err := pool.QueryRow(context.Background(), `INSERT INTO issue_delegation_return(id,workspace_id,issue_id,dispatcher_agent_id,return_kind,external_session_id) VALUES(gen_random_uuid(),$1,$2,$3,'external_session',$4) ON CONFLICT(workspace_id,issue_id,dispatcher_agent_id) DO NOTHING RETURNING id::text`, rollbackWorkspace, rollbackIssue, rollbackAgent, rollbackSession).Scan(&inserted)
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("old writer conflict inference failed: %v", err)
				}
			}
			if err := runTakeoverMigrations(t, pool, opts, resultTakeoverMigrationVersions, "up"); err != nil {
				t.Fatal(err)
			}
			if got := takeoverFacts(t, pool); got != before {
				t.Fatal("re-upgrade changed ordinary facts")
			}
			assertIndexValidity(t, pool, schema, "uq_current_issue_delegation_return", true)
			assertIndexExists(t, pool, schema, "uq_issue_delegation_return_dispatch", false)
		})
	}
}

func TestResultTakeoverRollbackRefusesHistoryBeforeAnyChange(t *testing.T) {
	for _, state := range []string{"action", "orphan_obligation", "historical_version", "duplicate_legacy_key"} {
		t.Run(state, func(t *testing.T) {
			_, pool, opts := takeoverRollbackFixture(t)
			seedOrdinaryTakeoverFacts(t, pool)
			if err := runTakeoverMigrations(t, pool, opts, resultTakeoverMigrationVersions, "up"); err != nil {
				t.Fatal(err)
			}
			switch state {
			case "action":
				seedTakeoverAction(t, pool, true)
			case "orphan_obligation":
				if _, err := pool.Exec(context.Background(), `UPDATE agent_task_delegation_return SET takeover_id=$1`, rollbackAction); err != nil {
					t.Fatal(err)
				}
			case "historical_version":
				if _, err := pool.Exec(context.Background(), `UPDATE issue_delegation_return SET is_current=false`); err != nil {
					t.Fatal(err)
				}
			case "duplicate_legacy_key":
				if _, err := pool.Exec(context.Background(), `DROP INDEX uq_current_issue_delegation_return`); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(context.Background(), `INSERT INTO issue_delegation_return(id,workspace_id,issue_id,dispatcher_agent_id,return_kind,external_session_id) SELECT gen_random_uuid(),workspace_id,issue_id,dispatcher_agent_id,return_kind,external_session_id FROM issue_delegation_return`); err != nil {
					t.Fatal(err)
				}
			}
			before := takeoverSchemaAndHistory(t, pool)
			err := runTakeoverMigrations(t, pool, opts, resultTakeoverMigrationVersions, "down")
			if err == nil || !strings.Contains(err.Error(), "cannot roll back result takeover") {
				t.Fatalf("unsafe rollback not refused: %v", err)
			}
			if got := takeoverSchemaAndHistory(t, pool); got != before {
				t.Fatal("refused rollback changed schema/proof/IDs/receipt")
			}
			// Both destructive SQL files also refuse without runner hooks.
			for _, version := range []string{resultTakeoverMigrationVersions[4], resultTakeoverMigrationVersions[0]} {
				sql, err := os.ReadFile(realMigrationFiles(t, []string{version}, "down")[0])
				if err != nil {
					t.Fatal(err)
				}
				if _, err = pool.Exec(context.Background(), string(sql)); err == nil {
					t.Fatalf("direct %s SQL erased history", version)
				}
				if got := takeoverSchemaAndHistory(t, pool); got != before {
					t.Fatal("direct refused SQL changed facts")
				}
			}
		})
	}
}

func TestResultTakeoverRollbackGuardsPartialUpgradeEntries(t *testing.T) {
	for end, version := range resultTakeoverMigrationVersions {
		t.Run(version, func(t *testing.T) {
			_, pool, opts := takeoverRollbackFixture(t)
			seedOrdinaryTakeoverFacts(t, pool)
			versions := resultTakeoverMigrationVersions[:end+1]
			if err := runTakeoverMigrations(t, pool, opts, versions, "up"); err != nil {
				t.Fatal(err)
			}
			seedTakeoverAction(t, pool, end == 4)
			before := takeoverSchemaAndHistory(t, pool)
			if err := runTakeoverMigrations(t, pool, opts, versions, "down"); err == nil || !strings.Contains(err.Error(), "cannot roll back result takeover") {
				t.Fatalf("partial rollback erased history: %v", err)
			}
			if got := takeoverSchemaAndHistory(t, pool); got != before {
				t.Fatal("partial refusal changed schema or facts")
			}
		})
	}
}

func TestResultTakeoverRollbackGuardPrecedesInvalidIndexCleanup(t *testing.T) {
	schema, pool, opts := takeoverRollbackFixture(t)
	seedOrdinaryTakeoverFacts(t, pool)
	versions := resultTakeoverMigrationVersions[:3]
	if err := runTakeoverMigrations(t, pool, opts, versions, "up"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO issue_delegation_return(id,workspace_id,issue_id,dispatcher_agent_id,return_kind,external_session_id,is_current) SELECT gen_random_uuid(),workspace_id,issue_id,dispatcher_agent_id,return_kind,external_session_id,false FROM issue_delegation_return`); err != nil {
		t.Fatal(err)
	}
	restore, err := os.ReadFile(realMigrationFiles(t, []string{versions[2]}, "down")[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(context.Background(), string(restore)); err == nil {
		t.Fatal("duplicate fixture did not leave invalid concurrent index")
	}
	assertIndexValidity(t, pool, schema, "uq_issue_delegation_return_dispatch", false)
	before := takeoverSchemaAndHistory(t, pool)
	if err := runTakeoverMigrations(t, pool, opts, []string{versions[2]}, "down"); err == nil {
		t.Fatal("history not refused before cleanup")
	}
	if got := takeoverSchemaAndHistory(t, pool); got != before {
		t.Fatal("cleanup destroyed an index before history refusal")
	}
}

func TestResultTakeoverRollbackRepairsInterruptedUnusedIndex(t *testing.T) {
	schema, pool, opts := takeoverRollbackFixture(t)
	seedOrdinaryTakeoverFacts(t, pool)
	versions := resultTakeoverMigrationVersions[:3]
	if err := runTakeoverMigrations(t, pool, opts, versions, "up"); err != nil {
		t.Fatal(err)
	}
	before := takeoverFacts(t, pool)
	blocker, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err = blocker.Exec(context.Background(), `UPDATE issue_delegation_return SET created_at=created_at WHERE id=$1`, rollbackReturn); err != nil {
		t.Fatal(err)
	}
	restore, err := os.ReadFile(realMigrationFiles(t, []string{versions[2]}, "down")[0])
	if err != nil {
		t.Fatal(err)
	}
	builder, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = builder.Exec(context.Background(), `SET statement_timeout='1s'`); err != nil {
		builder.Release()
		t.Fatal(err)
	}
	_, buildErr := builder.Exec(context.Background(), string(restore))
	_, resetErr := builder.Exec(context.Background(), `SET statement_timeout=DEFAULT`)
	builder.Release()
	if resetErr != nil {
		t.Fatal(resetErr)
	}
	if buildErr == nil {
		t.Fatal("index build was not interrupted")
	}
	if err := blocker.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertIndexValidity(t, pool, schema, "uq_issue_delegation_return_dispatch", false)
	if err := runTakeoverMigrations(t, pool, opts, versions, "down"); err != nil {
		t.Fatal(err)
	}
	assertIndexValidity(t, pool, schema, "uq_issue_delegation_return_dispatch", true)
	if got := takeoverFacts(t, pool); got != before {
		t.Fatal("index retry changed retained facts")
	}
}

func TestResultTakeoverRollbackBundleCLIStopsAt638(t *testing.T) {
	bundle := t.TempDir()
	dir := filepath.Join(bundle, "migrations")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, file := range realMigrationFiles(t, resultTakeoverMigrationVersions, "down") {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(file)), data, 0600); err != nil {
			t.Fatal(err)
		}
		t.Logf("bundle %s sha256=%x", filepath.Base(file), sha256.Sum256(data))
	}
	binary := filepath.Join(bundle, "migrate")
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "GOFLAGS=-buildvcs=false -p=1", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build current runner: %v %s", err, output)
	}
	for _, history := range []bool{false, true} {
		t.Run(fmt.Sprintf("history_%t", history), func(t *testing.T) {
			schema, pool, opts := takeoverRollbackFixture(t)
			seedOrdinaryTakeoverFacts(t, pool)
			if err := runTakeoverMigrations(t, pool, opts, resultTakeoverMigrationVersions, "up"); err != nil {
				t.Fatal(err)
			}
			if history {
				seedTakeoverAction(t, pool, true)
			}
			before := takeoverSchemaAndHistory(t, pool)
			facts := takeoverFacts(t, pool)
			dbURL, err := url.Parse(testDatabaseURL())
			if err != nil {
				t.Fatal(err)
			}
			values := dbURL.Query()
			values.Set("search_path", schema)
			dbURL.RawQuery = values.Encode()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, binary, "down")
			command.Dir = bundle
			command.Env = append(os.Environ(), "DATABASE_URL="+dbURL.String())
			output, runErr := command.CombinedOutput()
			if history {
				if runErr == nil || !strings.Contains(string(output), "cannot roll back result takeover") {
					t.Fatalf("CLI failed to refuse history: %v %s", runErr, output)
				}
				if got := takeoverSchemaAndHistory(t, pool); got != before {
					t.Fatal("CLI refusal changed schema/history fingerprint")
				}
				return
			}
			if runErr != nil {
				t.Fatalf("bounded CLI rollback: %v %s", runErr, output)
			}
			var last string
			if err := pool.QueryRow(context.Background(), `SELECT max(version) FROM schema_migrations`).Scan(&last); err != nil || last != "638_delegation_result_attempt_time" {
				t.Fatalf("CLI did not stop at638: %s %v", last, err)
			}
			if got := takeoverFacts(t, pool); got != facts {
				t.Fatal("bounded CLI rollback changed ordinary facts")
			}
			t.Logf("real CLI stopped with ledger=%s", last)
		})
	}
}
