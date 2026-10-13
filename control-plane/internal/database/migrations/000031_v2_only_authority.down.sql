-- HOR-582: reverse the V2-only authority schema for local development only.
--
-- This restores the *schema shape* migrations 000027 and earlier expect, so a
-- development rollback can continue. It deliberately does not restore revoked
-- legacy credentials, the `user` role spelling, IdentityMapping bindings, or
-- PermissionPolicy rows: V2 authority is one-way (DES-HOR-582-02) and
-- recreating legacy authority would reintroduce a second customer writer.

DROP VIEW IF EXISTS identity.inference_api_credentials;
DROP VIEW IF EXISTS identity.effective_api_credentials;

-- ---------------------------------------------------------------------------
-- Credential columns and constraints
-- ---------------------------------------------------------------------------

ALTER TABLE identity.api_keys DROP CONSTRAINT IF EXISTS api_keys_v2_complete;
DROP INDEX IF EXISTS identity.api_keys_owner;
DROP INDEX IF EXISTS identity.api_keys_actor;

ALTER TABLE identity.api_keys ADD COLUMN scope text;
ALTER TABLE identity.api_keys ADD CONSTRAINT api_keys_scope_shape
    CHECK (scope IS NULL OR scope IN ('admin', 'token', 'gateway', 'work'));
ALTER TABLE identity.api_keys ADD COLUMN credential_epoch text NOT NULL DEFAULT 'legacy'
    CHECK (credential_epoch IN ('legacy', 'v2'));
UPDATE identity.api_keys SET credential_epoch = 'v2' WHERE key_type IS NOT NULL;

ALTER TABLE identity.api_keys ADD CONSTRAINT api_keys_v2_complete CHECK (
    credential_epoch <> 'v2' OR (
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
    WHERE credential_epoch = 'v2' AND status <> 'revoked';
CREATE INDEX api_keys_actor ON identity.api_keys (actor_identity_id)
    WHERE credential_epoch = 'v2' AND status <> 'revoked';

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
           k.credential_epoch          AS credential_epoch,
           (k.credential_status = 'active'
                AND (k.expires_at IS NULL OR k.expires_at > now())
                AND k.owner_status = 'active'
                AND (k.key_type = 'personal'   AND k.owner_role IN ('admin', 'operator')
                  OR k.key_type = 'automation' AND k.owner_role = 'admin')) AS eligible,
           CASE
                WHEN k.credential_epoch <> 'v2'                            THEN 'legacy_epoch'
                WHEN k.credential_status = 'revoked'                       THEN 'revoked'
                WHEN k.credential_status = 'expired'                       THEN 'expired'
                WHEN k.credential_status = 'suspended'                     THEN COALESCE(k.suspension_reason, 'suspended')
                WHEN k.expires_at IS NOT NULL AND k.expires_at <= now()    THEN 'expired'
                WHEN k.credential_status <> 'active'                       THEN k.credential_status
                WHEN k.owner_status <> 'active'                            THEN 'owner_inactive'
                WHEN k.key_type = 'automation' AND k.owner_role <> 'admin' THEN 'owner_not_admin'
                WHEN k.owner_role NOT IN ('admin', 'operator')             THEN 'owner_no_role'
                ELSE NULL
           END                          AS suspension_reason
    FROM (
        SELECT c.id, c.credential_family_id, c.credential_version, c.key_hash, c.key_type,
               c.identity_id, c.owner_user_identity_id, c.actions, c.expires_at,
               c.rate_rpm, c.rate_tpm, c.credential_epoch,
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
        WHERE c.credential_epoch = 'v2' AND c.key_type IS NOT NULL
    ) k;

CREATE VIEW identity.inference_api_credentials AS
    SELECT c.api_key_id, c.key_hash, c.key_type, c.credential_status, c.expires_at,
           ('inference.models.read' = ANY (c.actions)) AS can_read_models,
           ('inference.chat.invoke' = ANY (c.actions)) AS can_invoke_chat,
           c.owner_identity_id, c.owner_status, c.owner_role,
           c.actor_identity_id, c.actor_kind, c.actor_role,
           c.rate_rpm, c.rate_tpm, c.credential_epoch, c.eligible, c.suspension_reason
    FROM identity.effective_api_credentials c;

CREATE OR REPLACE VIEW identity.active_api_keys AS
    SELECT key_hash, identity_id
    FROM identity.api_keys
    WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now());

-- ---------------------------------------------------------------------------
-- Retired tables (empty shapes only)
-- ---------------------------------------------------------------------------

CREATE TABLE identity.authority_state (
    id                 boolean PRIMARY KEY DEFAULT true CHECK (id),
    epoch              text NOT NULL DEFAULT 'legacy' CHECK (epoch IN ('legacy', 'v2')),
    source_fingerprint text,
    preflight_at       timestamptz,
    preflight_result   jsonb NOT NULL DEFAULT '{}'::jsonb,
    cutover_id         text,
    cutover_at         timestamptz,
    cutover_operator   text,
    cutover_release    text,
    cutover_result     jsonb NOT NULL DEFAULT '{}'::jsonb,
    verified_at        timestamptz,
    verification_result jsonb NOT NULL DEFAULT '{}'::jsonb,
    updated_at         timestamptz NOT NULL DEFAULT now()
);
INSERT INTO identity.authority_state (id, epoch) VALUES (true, 'v2');
CREATE TRIGGER authority_state_updated BEFORE UPDATE ON identity.authority_state
    FOR EACH ROW EXECUTE FUNCTION identity.set_updated_at();

CREATE TABLE identity.external_mappings (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    identity_id uuid NOT NULL REFERENCES identity.identities(id) ON DELETE CASCADE,
    provider    text NOT NULL CHECK (provider IN ('slack', 'teams')),
    type        text NOT NULL CHECK (type IN ('user', 'group')),
    external_id text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider, type, external_id)
);
CREATE INDEX idx_external_mappings_identity ON identity.external_mappings (identity_id);
CREATE INDEX idx_external_mappings_resolve ON identity.external_mappings (provider, type, external_id);

CREATE TABLE permissions.policies (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    key          text NOT NULL,
    subject_kind text NOT NULL CHECK (subject_kind IN ('user', 'group', 'service_account', 'workflow')),
    subject_key  text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    deleted_at   timestamptz,
    rate_limits  jsonb,
    UNIQUE (key)
);
CREATE INDEX idx_policies_subject ON permissions.policies (subject_kind, subject_key);

CREATE OR REPLACE FUNCTION permissions.set_updated_at() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$;
CREATE TRIGGER policies_updated BEFORE UPDATE ON permissions.policies
    FOR EACH ROW EXECUTE FUNCTION permissions.set_updated_at();
CREATE TRIGGER policies_notify AFTER INSERT OR UPDATE OR DELETE ON permissions.policies
    FOR EACH ROW EXECUTE FUNCTION permissions.notify_change();

DROP VIEW IF EXISTS permissions.effective_rate_limits;
CREATE VIEW permissions.effective_rate_limits AS
    SELECT i.id   AS identity_id,
           MIN((p.rate_limits->>'rpm')::int) AS rpm,
           MIN((p.rate_limits->>'tpm')::int) AS tpm
    FROM permissions.policies p
    JOIN identity.identities i ON i.key = p.subject_key
    WHERE p.deleted_at IS NULL
      AND i.deleted_at IS NULL
      AND p.rate_limits IS NOT NULL
    GROUP BY i.id;

CREATE OR REPLACE VIEW permissions.effective_capabilities AS
    SELECT i.id     AS identity_id,
           '*'::text AS resource,
           '*'::text AS action
    FROM identity.identities i
    WHERE i.deleted_at IS NULL;

ALTER TABLE identity.local_users DROP CONSTRAINT IF EXISTS local_users_role_check;
ALTER TABLE identity.local_users ADD CONSTRAINT local_users_role_check
    CHECK (role IN ('admin', 'operator', 'user'));
ALTER TABLE identity.local_users ALTER COLUMN role SET DEFAULT 'user';
