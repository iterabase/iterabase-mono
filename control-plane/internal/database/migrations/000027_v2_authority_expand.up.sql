-- HOR-454: V2 customer authority contract (expand phase).
--
-- Authority: docs/architecture/v2-authentication-authority.md (HOR-451,
-- DES-HOR-451-01..14). HOR-453 delivered the browser journey and identity
-- state; this migration adds the target *authority* contract:
--
--   * the durable `identity.authority_state` epoch (DES-HOR-451-12),
--   * the personal/automation API-credential owner/actor/action substrate
--     (DES-HOR-451-01/02/13, architecture 7.7-7.8),
--   * the permanent ownership-history ledger (architecture 7.8),
--   * the bounded `identity.inference_api_credentials` read projection and the
--     operator-managed `catalog.effective_api_catalog` (architecture 7.12-7.13),
--   * the append-only, payload-free `usage.inference_events` ledger
--     (architecture 7.14).
--
-- This is the EXPAND phase only: it never switches authority, rewrites roles,
-- or revokes legacy keys. The irreversible epoch flip, the `admin|user` ->
-- `admin|operator` rewrite, and the legacy key disposition happen inside the
-- single locked `api authority cutover` transaction (architecture 15.4), which
-- also revokes the legacy gateway grants materialized by migrations 000008 and
-- 000023 and materializes the exact `DES-HOR-451-14` grant union.

-- ---------------------------------------------------------------------------
-- Durable V2 authority epoch (architecture 7.11, 15.4)
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

INSERT INTO identity.authority_state (id, epoch) VALUES (true, 'legacy');

CREATE TRIGGER authority_state_updated BEFORE UPDATE ON identity.authority_state
    FOR EACH ROW EXECUTE FUNCTION identity.set_updated_at();

-- Legacy `scope` becomes an inert pre-epoch column: V2 rows never set it, the
-- cutover drops it, and the shape check keeps every pre-epoch row valid.
ALTER TABLE identity.api_keys ALTER COLUMN scope DROP NOT NULL;
ALTER TABLE identity.api_keys DROP CONSTRAINT IF EXISTS api_keys_scope_check;
ALTER TABLE identity.api_keys ADD CONSTRAINT api_keys_scope_shape
    CHECK (scope IS NULL OR scope IN ('admin', 'token', 'gateway', 'work'));

-- ---------------------------------------------------------------------------
-- API credentials: personal/automation type, accountable owner, request actor,
-- immutable actions, expiry, rate policy, suspension, rotation lineage.
-- Every column is nullable for pre-epoch rows: the expand phase must leave
-- legacy rows valid and untouched (architecture 15.1). The cutover backfills
-- them and the V2-equality constraints make a half-mapped V2 row impossible.
-- ---------------------------------------------------------------------------

ALTER TABLE identity.api_keys
    ADD COLUMN credential_family_id   uuid,
    ADD COLUMN credential_version     integer NOT NULL DEFAULT 1,
    ADD COLUMN key_type               text CHECK (key_type IN ('personal', 'automation')),
    ADD COLUMN owner_user_identity_id uuid REFERENCES identity.identities(id),
    ADD COLUMN actor_identity_id      uuid REFERENCES identity.identities(id),
    ADD COLUMN actions                text[],
    ADD COLUMN purpose                text NOT NULL DEFAULT '',
    ADD COLUMN status                 text NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'retiring', 'suspended', 'expired', 'revoked')),
    ADD COLUMN suspension_reason      text,
    ADD COLUMN revocation_reason      text,
    ADD COLUMN rate_rpm               integer CHECK (rate_rpm IS NULL OR rate_rpm > 0),
    ADD COLUMN rate_tpm               integer CHECK (rate_tpm IS NULL OR rate_tpm > 0),
    ADD COLUMN retiring_until         timestamptz,
    ADD COLUMN predecessor_id         uuid REFERENCES identity.api_keys(id),
    ADD COLUMN created_by_identity_id uuid REFERENCES identity.identities(id),
    ADD COLUMN credential_epoch       text NOT NULL DEFAULT 'legacy'
        CHECK (credential_epoch IN ('legacy', 'v2'));

-- The fixed V2 action catalogue (architecture 9.1). Database checks reject any
-- wildcard, unknown, or type-inapplicable action; application code uses the
-- same catalogue, so neither layer can widen the other.
ALTER TABLE identity.api_keys
    ADD CONSTRAINT api_keys_actions_known CHECK (
        actions IS NULL OR (
            actions <@ ARRAY[
                'workflows.read', 'workflows.start', 'work.read', 'work.feedback.write',
                'artifacts.read', 'artifacts.upload', 'artifacts.delete',
                'inference.models.read', 'inference.chat.invoke',
                'people.read', 'value.read'
            ]::text[]
            AND NOT ('*' = ANY (actions))
        )
    ),
    ADD CONSTRAINT api_keys_actions_by_type CHECK (
        key_type IS NULL OR (
            key_type = 'personal' AND actions <@ ARRAY[
                'workflows.read', 'workflows.start', 'work.read', 'work.feedback.write',
                'artifacts.read', 'artifacts.upload', 'artifacts.delete',
                'inference.models.read', 'inference.chat.invoke',
                'people.read', 'value.read'
            ]::text[]
        ) OR (
            key_type = 'automation' AND actions <@ ARRAY[
                'workflows.read', 'workflows.start', 'work.read',
                'artifacts.read', 'artifacts.upload',
                'inference.models.read', 'inference.chat.invoke'
            ]::text[]
        )
    ),
    ADD CONSTRAINT api_keys_owner_actor CHECK (
        key_type IS NULL OR (
            key_type = 'personal'   AND owner_user_identity_id = actor_identity_id
        ) OR (
            key_type = 'automation' AND owner_user_identity_id <> actor_identity_id
        )
    ),
    -- A V2 credential must be fully specified: accountable human owner, request
    -- actor, at least one action, materialized rates, and a mandatory expiry
    -- (architecture 5.4: no never-expiring V2 key).
    ADD CONSTRAINT api_keys_v2_complete CHECK (
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

-- Exactly one live version per credential family: at most one non-terminal
-- version (active, or a bounded retiring overlap) may exist.
CREATE UNIQUE INDEX api_keys_one_live_version
    ON identity.api_keys (credential_family_id)
    WHERE credential_family_id IS NOT NULL AND status IN ('active', 'retiring');

CREATE INDEX api_keys_owner ON identity.api_keys (owner_user_identity_id)
    WHERE credential_epoch = 'v2' AND status <> 'revoked';
CREATE INDEX api_keys_actor ON identity.api_keys (actor_identity_id)
    WHERE credential_epoch = 'v2' AND status <> 'revoked';
CREATE INDEX api_keys_retiring ON identity.api_keys (retiring_until)
    WHERE status = 'retiring';

-- ---------------------------------------------------------------------------
-- Permanent append-only ownership history (architecture 7.8)
-- ---------------------------------------------------------------------------

CREATE TABLE identity.api_key_ownership_history (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    credential_family_id    uuid NOT NULL,
    credential_id           uuid NOT NULL,
    prior_owner_identity_id uuid NOT NULL,
    new_owner_identity_id   uuid NOT NULL,
    transferred_by_identity_id uuid NOT NULL,
    reason                  text,
    correlation_id          text,
    created_at              timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX api_key_ownership_history_family
    ON identity.api_key_ownership_history (credential_family_id, created_at);

-- ---------------------------------------------------------------------------
-- Operator-managed customer API model exposure (architecture 7.13)
--
-- `catalog.models` already carries the operator-reconciled alias metadata.
-- Customer API exposure is an explicit, separately rate-policied operator
-- decision: absence of `api_exposed` keeps the alias internal to the workload
-- surface. The customer projection below exposes no provider/backend detail.
-- ---------------------------------------------------------------------------

ALTER TABLE catalog.models
    ADD COLUMN api_exposed  boolean NOT NULL DEFAULT false,
    ADD COLUMN api_rate_rpm integer CHECK (api_rate_rpm IS NULL OR api_rate_rpm > 0),
    ADD COLUMN api_rate_tpm integer CHECK (api_rate_tpm IS NULL OR api_rate_tpm > 0);

CREATE VIEW catalog.effective_api_catalog AS
    SELECT m.model_id        AS alias,
           m.display_name    AS display_name,
           m.context_length  AS context_length,
           m.capabilities    AS capabilities,
           m.api_rate_rpm    AS rate_rpm,
           m.api_rate_tpm    AS rate_tpm,
           (m.api_exposed AND m.available AND b.healthy) AS enabled
    FROM catalog.models m
    JOIN catalog.backends b
      ON b.name = m.backend_ref AND b.namespace = m.namespace AND b.deleted_at IS NULL
    WHERE m.deleted_at IS NULL AND m.api_exposed;

-- ---------------------------------------------------------------------------
-- Live customer-credential authority projection (architecture 7.7, 7.12).
-- One indexed base-key lookup plus live joins: never a snapshot and never a
-- request-authority cache, so revocation, expiry, disablement, and demotion
-- take effect on the next request. The control-plane consumes this view; the
-- Inference Gateway consumes the bounded `inference_api_credentials` subset
-- below so it never gains the wider action payload.
-- ---------------------------------------------------------------------------

CREATE OR REPLACE VIEW identity.effective_api_credentials AS
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
               c.credential_epoch,
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

-- Bounded Inference Gateway projection: only what a customer request needs,
-- with boolean action membership instead of the raw action array.
CREATE OR REPLACE VIEW identity.inference_api_credentials AS
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
           c.credential_epoch,
           c.eligible,
           c.suspension_reason
    FROM identity.effective_api_credentials c;

-- ---------------------------------------------------------------------------
-- Append-only, payload-free inference usage ledger (architecture 7.14).
-- No prompt, response, tool content, provider credential, or raw Authorization
-- value may be represented here. No FK: the ledger is permanent evidence that
-- must outlive identity lifecycle changes.
-- ---------------------------------------------------------------------------

CREATE TABLE usage.inference_events (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id           text NOT NULL,
    request_id         text,
    correlation_id     text,
    phase              text NOT NULL CHECK (phase IN (
                           'accepted', 'completed', 'failed', 'canceled', 'outcome_unknown')),
    api_key_id         uuid NOT NULL,
    owner_identity_id  uuid NOT NULL,
    actor_identity_id  uuid NOT NULL,
    credential_kind    text NOT NULL CHECK (credential_kind IN ('personal', 'automation')),
    action             text NOT NULL CHECK (action IN ('inference.models.read', 'inference.chat.invoke')),
    model_alias        text,
    stream             boolean NOT NULL DEFAULT false,
    status             integer,
    error_class        text,
    input_tokens       integer CHECK (input_tokens IS NULL OR input_tokens >= 0),
    output_tokens      integer CHECK (output_tokens IS NULL OR output_tokens >= 0),
    occurred_at        timestamptz NOT NULL DEFAULT now(),
    created_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (event_id)
);

CREATE INDEX inference_events_key ON usage.inference_events (api_key_id, created_at);
CREATE INDEX inference_events_actor ON usage.inference_events (actor_identity_id, created_at);
CREATE INDEX inference_events_owner ON usage.inference_events (owner_identity_id, created_at);
CREATE INDEX inference_events_request ON usage.inference_events (request_id)
    WHERE request_id IS NOT NULL;
