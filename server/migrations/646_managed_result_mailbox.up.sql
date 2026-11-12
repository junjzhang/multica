-- Apply with result writers quiesced. Old writers fail closed after this migration.
ALTER TABLE agent_task_delegation_return DROP CONSTRAINT delegation_result_single_transport;

INSERT INTO agent_mailbox_message (
 id, workspace_id, issue_id, recipient_agent_id, source_type, source_key,
 kind, notification_key, payload, created_at
)
SELECT gen_random_uuid(), r.workspace_id, r.issue_id, r.dispatcher_agent_id,
 'delegation_result', 'result:' || l.task_id || ':return:' || r.id,
 'delegation_result', 'result:' || l.task_id || ':return:' || r.id,
 jsonb_build_object('result_task_id', l.task_id, 'delegation_return_id', r.id,
                    'issue_id', r.issue_id, 'outcome', outcome.status),
 COALESCE(l.materialized_at, now())
FROM agent_task_delegation_return l
JOIN issue_delegation_return r ON r.id=l.delegation_return_id
JOIN agent_task_queue outcome ON outcome.id=l.task_id
WHERE l.notification_task_id IS NOT NULL AND l.notification_delivery_id IS NULL
ON CONFLICT DO NOTHING;

UPDATE agent_task_delegation_return l
SET notification_delivery_id=m.id
FROM agent_mailbox_message m
WHERE l.notification_task_id IS NOT NULL AND l.notification_delivery_id IS NULL
 AND m.notification_key='result:' || l.task_id || ':return:' || l.delegation_return_id;

ALTER TABLE agent_task_delegation_return ADD CONSTRAINT managed_result_requires_mailbox
 CHECK (notification_task_id IS NULL OR notification_delivery_id IS NOT NULL);
