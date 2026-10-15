-- HOR-661: reverse 000033. Historical managed backend rows are kept: the
-- restored kind constraint is NOT VALID, so it binds new writes only and a
-- rollback never deletes catalog history.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'gateway') THEN
        REVOKE SELECT (id, work_item_id) ON work.attempts FROM gateway;
        REVOKE USAGE ON SCHEMA work FROM gateway;
    END IF;
END $$;

DROP SCHEMA IF EXISTS ratelimit CASCADE;

ALTER TABLE catalog.backends DROP CONSTRAINT backends_kind_check;
ALTER TABLE catalog.backends ADD CONSTRAINT backends_kind_check
    CHECK (kind IN ('vLLM', 'SGLang', 'external')) NOT VALID;
