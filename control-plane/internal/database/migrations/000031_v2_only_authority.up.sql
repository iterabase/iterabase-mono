-- HOR-582: V2-only customer authority (DES-HOR-582-01, DES-HOR-582-02).
--
-- Every installation runs V2 authority through one path. This forward
-- migration replaces the earlier expand -> preflight -> cutover lifecycle: it
-- applies the complete V2 authority state in one transaction and removes every
-- legacy customer-authority object, so no binary can observe a legacy epoch.
--
--   * local roles are exactly admin|operator (`user` -> `operator`);
--   * every legacy scoped API key (`admin|token|gateway|work`) is revoked and
--     can never be valid again: only fully specified V2 credentials are live;
--   * the authority epoch, the IdentityMapping external-binding table, and the
--     PermissionPolicy table retire with their writers;
--   * the broad `*/*` capability applies only to non-human workload identities;
--   * the legacy gateway customer lookup is dropped and the dedicated gateway
--     role holds exactly the DES-HOR-451-14 read union.
--
-- Work, artifact, attribution, and security-audit history is untouched. The
-- transition is one-way: rollback is roll-forward or catastrophic restore.

-- ---------------------------------------------------------------------------
-- Local roles
-- ---------------------------------------------------------------------------

UPDATE identity.local_users SET role = 'operator', role_changed_at = now() WHERE role = 'user';
ALTER TABLE identity.local_users ALTER COLUMN role SET DEFAULT 'operator';
ALTER TABLE identity.local_users DROP CONSTRAINT IF EXISTS local_users_role_check;
ALTER TABLE identity.local_users ADD CONSTRAINT local_users_role_check
    CHECK (role IN ('admin', 'operator'));

-- ---------------------------------------------------------------------------
-- Legacy credentials: revoke, then make a live legacy key unrepresentable
-- ---------------------------------------------------------------------------

DROP VIEW IF EXISTS identity.active_api_keys;
DROP VIEW IF EXISTS identity.inference_api_credentials;
DROP VIEW IF EXISTS identity.effective_api_credentials;

UPDATE identity.api_keys
SET revoked_at = COALESCE(revoked_at, now()), status = 'revoked',
    revocation_reason = COALESCE(revocation_reason, 'legacy_authority')
WHERE key_type IS NULL OR credential_epoch <> 'v2';

ALTER TABLE identity.api_keys DROP CONSTRAINT IF EXISTS api_keys_scope_shape;
ALTER TABLE identity.api_keys DROP CONSTRAINT IF EXISTS api_keys_v2_complete;
DROP INDEX IF EXISTS identity.api_keys_owner;
DROP INDEX IF EXISTS identity.api_keys_actor;
ALTER TABLE identity.api_keys DROP COLUMN scope;
ALTER TABLE identity.api_keys DROP COLUMN credential_epoch;

-- A row is either a fully specified V2 credential or revoked legacy history.
ALTER TABLE identity.api_keys ADD CONSTRAINT api_keys_v2_complete CHECK (
    (key_type IS NULL AND revoked_at IS NOT NULL AND status = 'revoked') OR (
        key_type IS NOT NULL
        AND owner_user_identity_id IS NOT NULL
        AND actor_identity_id IS NOT NULL
        AND actions IS NOT NULL AND cardinality(actions) > 0
        AND rate_rpm IS NOT NULL AND rate_tpm IS NOT NULL
        AND expires_at IS NOT NULL
        AND credential_family_id IS NOT NULL
    )
);

CREATE INDEX api_keys_owner ON identity.api_keys (owner_user_identity_id)
    WHERE key_type IS NOT NULL AND status <> 'revoked';
CREATE INDEX api_keys_actor ON identity.api_keys (actor_identity_id)
    WHERE key_type IS NOT NULL AND status <> 'revoked';

-- Live customer-credential authority projection (architecture 7.7, 7.12): one
-- indexed base-key lookup plus live joins, never a snapshot or cache.
CREATE VIEW identity.effective_api_credentials AS
    SELECT k.id                        AS api_key_id,
           k.credential_family_id      AS credential_family_id,
           k.credential_version        AS credential_version,
           k.key_hash                  AS key_hash,
           k.key_type                  AS key_type,
           k.credential_status         AS credential_status,
           k.expires_at                AS expires_at,
           k.actions                   AS actions,
           k.owner_user_identity_id    AS owner_identity_id,
           k.owner_status              AS owner_status,
           k.owner_role                AS owner_role,
           k.identity_id               AS actor_identity_id,
           k.actor_kind                AS actor_kind,
           k.actor_role                AS actor_role,
           k.rate_rpm                  AS rate_rpm,
           k.rate_tpm                  AS rate_tpm,
           (k.credential_status = 'active'
                AND k.expires_at > now()
                AND k.owner_status = 'active'
                AND (k.key_type = 'personal'   AND k.owner_role IN ('admin', 'operator')
                  OR k.key_type = 'automation' AND k.owner_role = 'admin')) AS eligible,
           CASE
                WHEN k.credential_status = 'revoked'                       THEN 'revoked'
                WHEN k.credential_status = 'expired'                       THEN 'expired'
                WHEN k.credential_status = 'suspended'                     THEN COALESCE(k.suspension_reason, 'suspended')
                WHEN k.expires_at <= now()                                 THEN 'expired'
                WHEN k.credential_status <> 'active'                       THEN k.credential_status
                WHEN k.owner_status <> 'active'                            THEN 'owner_inactive'
                WHEN k.key_type = 'automation' AND k.owner_role <> 'admin' THEN 'owner_not_admin'
                WHEN k.owner_role NOT IN ('admin', 'operator')             THEN 'owner_no_role'
                ELSE NULL
           END                          AS suspension_reason
    FROM (
        SELECT c.id,
               c.credential_family_id,
               c.credential_version,
               c.key_hash,
               c.key_type,
               c.identity_id,
               c.owner_user_identity_id,
               c.actions,
               c.expires_at,
               c.rate_rpm,
               c.rate_tpm,
               CASE
                   WHEN c.status = 'retiring'
                        AND c.retiring_until IS NOT NULL
                        AND c.retiring_until > now() THEN 'active'
                   ELSE c.status
               END AS credential_status,
               c.suspension_reason,
               COALESCE(lu.status, 'missing') AS owner_status,
               COALESCE(lu.role, '')          AS owner_role,
               ai.kind                        AS actor_kind,
               COALESCE(alu.role, '')         AS actor_role
        FROM identity.api_keys c
        JOIN identity.identities ai ON ai.id = c.identity_id AND ai.deleted_at IS NULL
        LEFT JOIN identity.local_users lu  ON lu.identity_id = c.owner_user_identity_id
        LEFT JOIN identity.local_users alu ON alu.identity_id = c.identity_id
        WHERE c.key_type IS NOT NULL
    ) k;

-- Bounded Inference Gateway projection (consumed by HOR-513).
CREATE VIEW identity.inference_api_credentials AS
    SELECT c.api_key_id,
           c.key_hash,
           c.key_type,
           c.credential_status,
           c.expires_at,
           ('inference.models.read' = ANY (c.actions)) AS can_read_models,
           ('inference.chat.invoke' = ANY (c.actions)) AS can_invoke_chat,
           c.owner_identity_id,
           c.owner_status,
           c.owner_role,
           c.actor_identity_id,
           c.actor_kind,
           c.actor_role,
           c.rate_rpm,
           c.rate_tpm,
           c.eligible,
           c.suspension_reason
    FROM identity.effective_api_credentials c;

-- ---------------------------------------------------------------------------
-- Retired legacy authority objects
-- ---------------------------------------------------------------------------

DROP TABLE IF EXISTS identity.authority_state;
DROP TABLE IF EXISTS identity.external_mappings;

-- PermissionPolicy storage retires with its reconciler. The rate-limit view
-- keeps its typed contract for the gateway workload pipeline; no customer or
-- workload policy writer exists, so it is empty (absent row = no per-identity
-- workload limit, as before for identities without a policy).
DROP VIEW IF EXISTS permissions.effective_rate_limits;
DROP TABLE IF EXISTS permissions.policies;
DROP FUNCTION IF EXISTS permissions.set_updated_at();

CREATE VIEW permissions.effective_rate_limits AS
    SELECT NULL::uuid AS identity_id, NULL::int AS rpm, NULL::int AS tpm
    WHERE false;

-- Broad workload capability only: humans hold customer authority through
-- browser sessions and V2 credentials, never through a wildcard grant.
CREATE OR REPLACE VIEW permissions.effective_capabilities AS
    SELECT i.id     AS identity_id,
           '*'::text AS resource,
           '*'::text AS action
    FROM identity.identities i
    WHERE i.deleted_at IS NULL
      AND i.kind NOT IN ('user', 'group');

-- ---------------------------------------------------------------------------
-- Exact DES-HOR-451-14 gateway grant union (architecture 7.12)
-- ---------------------------------------------------------------------------

DO $$
DECLARE
    cur_user text := current_user;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'gateway') THEN
        RETURN;
    END IF;

    -- Stop future objects from inheriting the legacy schema-wide default
    -- (migration 000008) and revoke every already-materialized privilege, so
    -- the union below is the complete set.
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA identity    REVOKE SELECT ON TABLES FROM gateway', cur_user);
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA permissions REVOKE SELECT ON TABLES FROM gateway', cur_user);
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA catalog     REVOKE SELECT ON TABLES FROM gateway', cur_user);
    REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA identity, permissions, catalog, toolgateway, runtime, usage FROM gateway;

    GRANT USAGE ON SCHEMA identity, catalog, usage, permissions, toolgateway, runtime TO gateway;
    REVOKE CREATE ON SCHEMA identity, catalog, usage, permissions, toolgateway, runtime FROM gateway;

    -- Customer authority: bounded projection reads plus the payload-free usage
    -- ledger. Never a mutation or raw-table read.
    GRANT SELECT ON identity.inference_api_credentials, catalog.effective_api_catalog TO gateway;
    GRANT INSERT ON usage.inference_events TO gateway;

    -- Separately approved non-customer routing/workload reads.
    GRANT SELECT ON catalog.effective_catalog,
                    permissions.effective_capabilities,
                    permissions.effective_rate_limits,
                    toolgateway.pools,
                    runtime.turns,
                    runtime.workflow_runs,
                    runtime.run_pool_assignments,
                    runtime.turn_assignments
        TO gateway;
END $$;
