DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM agent_mailbox_message WHERE handled_by_task_id IS NOT NULL) THEN
  RAISE EXCEPTION 'mailbox consumer provenance requires forward recovery';
 END IF;
END $$;
ALTER TABLE agent_mailbox_message DROP COLUMN handled_by_task_id;
