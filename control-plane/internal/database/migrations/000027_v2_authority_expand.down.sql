-- HOR-454: reverse the V2 authority expand phase.
--
-- This reverses the *schema* only. It deliberately does not restore the legacy
-- `admin|user` role spelling or any revoked legacy credential: the V2 authority
-- epoch is irreversible (DES-HOR-451-12) and re-creating legacy authority would
-- reintroduce the split-brain writer the cutover removes. Re-widening the role
-- check keeps the pre-epoch schema usable for local development rehearsals.

-- ---------------------------------------------------------------------------
-- Projections and catalogue first: they depend on the credential columns below.
-- ---------------------------------------------------------------------------

DROP VIEW IF EXISTS identity.inference_api_credentials;
DROP VIEW IF EXISTS identity.effective_api_credentials;

DROP VIEW IF EXISTS catalog.effective_api_catalog;
ALTER TABLE catalog.models
    DROP COLUMN IF EXISTS api_exposed,
    DROP COLUMN IF EXISTS api_rate_rpm,
    DROP COLUMN IF EXISTS api_rate_tpm;

DROP TABLE IF EXISTS usage.inference_events;

-- ---------------------------------------------------------------------------
-- Legacy key scope restored (the cutover drops it; a pre-epoch row needs it).
-- ---------------------------------------------------------------------------

ALTER TABLE identity.api_keys ADD COLUMN IF NOT EXISTS scope text;
UPDATE identity.api_keys
SET scope = 'work'
WHERE scope IS NULL;
ALTER TABLE identity.api_keys ALTER COLUMN scope SET NOT NULL;
ALTER TABLE identity.api_keys DROP CONSTRAINT IF EXISTS api_keys_scope_shape;
ALTER TABLE identity.api_keys DROP CONSTRAINT IF EXISTS api_keys_scope_check;
ALTER TABLE identity.api_keys ADD CONSTRAINT api_keys_scope_check
    CHECK (scope IN ('admin', 'token', 'gateway', 'work'));

-- ---------------------------------------------------------------------------
-- V2 credential substrate
-- ---------------------------------------------------------------------------

DROP INDEX IF EXISTS identity.api_keys_one_live_version;
DROP INDEX IF EXISTS identity.api_keys_owner;
DROP INDEX IF EXISTS identity.api_keys_actor;
DROP INDEX IF EXISTS identity.api_keys_retiring;

ALTER TABLE identity.api_keys
    DROP CONSTRAINT IF EXISTS api_keys_actions_known,
    DROP CONSTRAINT IF EXISTS api_keys_actions_by_type,
    DROP CONSTRAINT IF EXISTS api_keys_owner_actor,
    DROP CONSTRAINT IF EXISTS api_keys_v2_complete,
    DROP COLUMN IF EXISTS credential_family_id,
    DROP COLUMN IF EXISTS credential_version,
    DROP COLUMN IF EXISTS key_type,
    DROP COLUMN IF EXISTS owner_user_identity_id,
    DROP COLUMN IF EXISTS actor_identity_id,
    DROP COLUMN IF EXISTS actions,
    DROP COLUMN IF EXISTS purpose,
    DROP COLUMN IF EXISTS status,
    DROP COLUMN IF EXISTS suspension_reason,
    DROP COLUMN IF EXISTS revocation_reason,
    DROP COLUMN IF EXISTS rate_rpm,
    DROP COLUMN IF EXISTS rate_tpm,
    DROP COLUMN IF EXISTS retiring_until,
    DROP COLUMN IF EXISTS predecessor_id,
    DROP COLUMN IF EXISTS created_by_identity_id,
    DROP COLUMN IF EXISTS credential_epoch;

DROP TABLE IF EXISTS identity.api_key_ownership_history;

-- ---------------------------------------------------------------------------
-- Epoch and local-role check
-- ---------------------------------------------------------------------------

DROP TABLE IF EXISTS identity.authority_state;

ALTER TABLE identity.local_users DROP CONSTRAINT IF EXISTS local_users_role_check;
ALTER TABLE identity.local_users ADD CONSTRAINT local_users_role_check
    CHECK (role IN ('admin', 'operator', 'user'));
