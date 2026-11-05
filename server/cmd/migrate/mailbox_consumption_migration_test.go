package main

import (
	"context"
	"math/rand/v2"
	"testing"
)

func TestManagedMailboxMigrationPreservesInflightAndRejectsOldWriter(t *testing.T) {
	ctx := context.Background()
	schema := createScratchSchema(t, ctx, openTestPool(t), "mailbox_consumption_")
	pool := openTestPoolWithSearchPath(t, schema)
	_, err := pool.Exec(ctx, `
 CREATE TABLE issue_delegation_return (LIKE public.issue_delegation_return INCLUDING DEFAULTS INCLUDING CONSTRAINTS);
 CREATE TABLE agent_task_queue(id UUID PRIMARY KEY,status TEXT);
 CREATE TABLE agent_task_delegation_return (LIKE public.agent_task_delegation_return INCLUDING DEFAULTS INCLUDING CONSTRAINTS);
 ALTER TABLE agent_task_delegation_return DROP CONSTRAINT managed_result_requires_mailbox;
 ALTER TABLE agent_task_delegation_return ADD CONSTRAINT delegation_result_single_transport CHECK (num_nonnulls(notification_task_id,notification_delivery_id)<=1);
 CREATE TABLE agent_mailbox_message (LIKE public.agent_mailbox_message INCLUDING DEFAULTS INCLUDING CONSTRAINTS);
 ALTER TABLE agent_mailbox_message DROP COLUMN handled_by_task_id;
 INSERT INTO issue_delegation_return(id,workspace_id,issue_id,dispatcher_agent_id,return_kind,external_session_id)
 VALUES ('11111111-1111-4111-8111-111111111111','22222222-2222-4222-8222-222222222222','33333333-3333-4333-8333-333333333333','44444444-4444-4444-8444-444444444444','external_session','88888888-8888-4888-8888-888888888888');
 INSERT INTO agent_task_queue VALUES ('55555555-5555-4555-8555-555555555555','completed');
 INSERT INTO agent_task_delegation_return(task_id,delegation_return_id,notification_task_id)
 VALUES ('55555555-5555-4555-8555-555555555555','11111111-1111-4111-8111-111111111111','77777777-7777-4777-8777-777777777777');
 INSERT INTO agent_mailbox_message(id,workspace_id,recipient_agent_id,target_session_id,source_type,kind,delivery_status,host_receipt)
 VALUES ('66666666-6666-4666-8666-666666666666','22222222-2222-4222-8222-222222222222','44444444-4444-4444-8444-444444444444','88888888-8888-4888-8888-888888888888','delegation_result','delegation_result','unknown','original-receipt');
 `)
	if err != nil {
		t.Fatal(err)
	}
	opts := runOptions{Direction: "up", Files: realMigrationFiles(t, []string{"646_managed_result_mailbox", "647_mailbox_result_consumer", "648_mailbox_result_delivery_index", "649_mailbox_result_consumer_index"}, "up"), SchemaMigrationsTable: schema + ".schema_migrations", AdvisoryLockKey: int64(rand.Uint64()&0x7fffffffffffffff) | 1, Hooks: hooksForDirection("up")}
	for range 2 {
		if err := runMigrations(ctx, pool, opts); err != nil {
			t.Fatal(err)
		}
	}
	var message, state, receipt string
	if err := pool.QueryRow(ctx, `SELECT notification_delivery_id::text FROM agent_task_delegation_return WHERE task_id=$1`, rollbackTask).Scan(&message); err != nil || message == "" {
		t.Fatalf("backfill: %q %v", message, err)
	}
	if err := pool.QueryRow(ctx, `SELECT delivery_status,host_receipt FROM agent_mailbox_message WHERE id=$1`, rollbackDelivery).Scan(&state, &receipt); err != nil || state != "unknown" || receipt != "original-receipt" {
		t.Fatalf("external receipt: %s %s %v", state, receipt, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_task_delegation_return SET notification_delivery_id=NULL WHERE task_id=$1`, rollbackTask); err == nil {
		t.Fatal("old writer could bypass mailbox")
	}
	down := opts
	down.Direction = "down"
	down.Files = realMigrationFiles(t, []string{"646_managed_result_mailbox"}, "down")
	down.Hooks = hooksForDirection("down")
	if err := runMigrations(ctx, pool, down); err == nil {
		t.Fatal("unsafe rollback discarded consumption contract")
	}
}
