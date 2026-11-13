-- Consumption cannot safely be rolled back to a writer that ignores ACKs.
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM agent_task_delegation_return WHERE notification_task_id IS NOT NULL) THEN
  RAISE EXCEPTION 'managed result mailbox records require forward recovery';
 END IF;
END $$;
ALTER TABLE agent_task_delegation_return DROP CONSTRAINT managed_result_requires_mailbox;
ALTER TABLE agent_task_delegation_return ADD CONSTRAINT delegation_result_single_transport
 CHECK (num_nonnulls(notification_delivery_id, notification_task_id) <= 1);
