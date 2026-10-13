-- HOR-597: reverse the backend auth_ref column. Dropping a view column needs a
-- DROP + CREATE, which loses the 000008 grant, so it is re-applied here.

DROP VIEW IF EXISTS catalog.effective_catalog;

CREATE VIEW catalog.effective_catalog AS
    SELECT m.key               AS model_key,
           m.namespace         AS namespace,
           m.model_id          AS model_id,
           m.display_name      AS display_name,
           m.context_length    AS context_length,
           m.capabilities      AS capabilities,
           m.backend_ref       AS backend_ref,
           b.kind              AS backend_kind,
           b.model             AS backend_model_id,
           b.service_url       AS backend_url,
           m.default_params    AS default_params,
           m.reasoning_config  AS reasoning_config,
           m.transforms        AS transforms,
           m.rate_limits       AS rate_limits,
           (m.available AND b.healthy) AS available
    FROM catalog.models m
    JOIN catalog.backends b
      ON b.name = m.backend_ref AND b.namespace = m.namespace AND b.deleted_at IS NULL
    WHERE m.deleted_at IS NULL;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'gateway') THEN
        GRANT SELECT ON catalog.effective_catalog TO gateway;
    END IF;
END $$;

ALTER TABLE catalog.backends DROP COLUMN IF EXISTS auth_ref;
