-- HOR-597 (DES-HOR-597-01): materialize an external ModelBackend's authRef — the
-- *name* of the Secret holding the provider credential, never its value — so the
-- inference gateway can resolve the credential from the Kubernetes API and send
-- it upstream. The view gains auth_ref as its last column (CREATE OR REPLACE
-- VIEW may only append columns), which keeps the gateway grant from 000008.

ALTER TABLE catalog.backends ADD COLUMN auth_ref text;

CREATE OR REPLACE VIEW catalog.effective_catalog AS
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
           (m.available AND b.healthy) AS available,
           b.auth_ref          AS backend_auth_ref
    FROM catalog.models m
    JOIN catalog.backends b
      ON b.name = m.backend_ref AND b.namespace = m.namespace AND b.deleted_at IS NULL
    WHERE m.deleted_at IS NULL;
