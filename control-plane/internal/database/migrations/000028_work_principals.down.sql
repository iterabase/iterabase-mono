-- HOR-454: reverse the three-principal seam columns. The recorded principals are
-- not restored into a legacy shape: dropping the columns removes the evidence
-- only in a development rollback.

ALTER TABLE runtime.workflow_runs
    DROP COLUMN IF EXISTS initiating_human_identity_id,
    DROP COLUMN IF EXISTS request_actor_identity_id,
    DROP COLUMN IF EXISTS authorization_source,
    DROP COLUMN IF EXISTS authorization_api_key_id,
    DROP COLUMN IF EXISTS authorization_session_id;

DROP INDEX IF EXISTS work.work_items_initiating_human;
DROP INDEX IF EXISTS work.work_items_request_actor;

ALTER TABLE work.work_items
    DROP COLUMN IF EXISTS initiating_human_identity_id,
    DROP COLUMN IF EXISTS request_actor_identity_id,
    DROP COLUMN IF EXISTS authorization_source,
    DROP COLUMN IF EXISTS authorization_api_key_id,
    DROP COLUMN IF EXISTS authorization_session_id;
