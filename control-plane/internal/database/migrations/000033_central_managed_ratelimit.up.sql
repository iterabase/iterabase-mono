-- HOR-661 (DES-HOR-661-03, DES-HOR-661-07).
--
-- 1. ModelBackend kind `managed`: the installation's inference gateway routes
--    these models to the central Iterabase gateway over mTLS. The catalog row
--    carries the central model name in `model` and an empty service_url; the
--    catalog views are kind-agnostic, so a healthy managed backend is available.
-- 2. Redis is removed from the platform: the inference gateway's per-identity
--    RPM/TPM windows move to `ratelimit.counters` in this database.
-- 3. Payload-free attribution: the gateway resolves the work item that drives a
--    run (work.attempts shares its id with runtime.workflow_runs) and sends it to
--    central. It may read only the two linking columns.

ALTER TABLE catalog.backends DROP CONSTRAINT backends_kind_check;
ALTER TABLE catalog.backends ADD CONSTRAINT backends_kind_check
    CHECK (kind IN ('vLLM', 'SGLang', 'external', 'managed'));

CREATE SCHEMA IF NOT EXISTS ratelimit;

-- One row per identity and one-minute window. Rows older than a few minutes
-- carry no meaning; the gateway deletes them.
CREATE TABLE ratelimit.counters (
    identity_id  text        NOT NULL,
    window_start timestamptz NOT NULL,
    requests     integer     NOT NULL DEFAULT 0,
    tokens       bigint      NOT NULL DEFAULT 0,
    PRIMARY KEY (identity_id, window_start)
);

CREATE INDEX idx_ratelimit_counters_window ON ratelimit.counters (window_start);

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'gateway') THEN
        GRANT USAGE ON SCHEMA ratelimit TO gateway;
        GRANT SELECT, INSERT, UPDATE, DELETE ON ratelimit.counters TO gateway;
        GRANT USAGE ON SCHEMA work TO gateway;
        GRANT SELECT (id, work_item_id) ON work.attempts TO gateway;
    END IF;
END $$;
