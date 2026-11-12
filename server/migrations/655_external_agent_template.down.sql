ALTER TABLE agent
    DROP COLUMN IF EXISTS external_host_id,
    DROP COLUMN IF EXISTS external_provider;
