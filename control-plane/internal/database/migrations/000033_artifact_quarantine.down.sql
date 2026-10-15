-- HOR-461: reverse the V2 artifact schema for local development only.
--
-- This restores the *schema shape* migration 000017 expects so a development
-- rollback can continue. It deliberately does not restore v1 immediate
-- availability for unscanned bytes: every non-deleted row comes back as
-- `pending`, which v1 never serves and its sweeper removes. The V2 artifact
-- epoch is one-way; production rollback is roll-forward or catastrophic
-- restore.

DROP TRIGGER artifact_link_serialize ON work.artifact_links;
DROP FUNCTION work.artifact_link_serialize();
DROP TRIGGER artifact_lifecycle ON artifact.artifacts;
DROP FUNCTION artifact.enforce_lifecycle();
DROP FUNCTION artifact.is_cleared(uuid);
DROP FUNCTION artifact.is_admissible_output(uuid);

ALTER TABLE artifact.artifacts
    ADD COLUMN storage_key  text,
    ADD COLUMN mime_type    text,
    ADD COLUMN state        text,
    ADD COLUMN available_at timestamptz;

UPDATE artifact.artifacts a SET
    storage_key = COALESCE(
        (SELECT o.storage_key FROM artifact.objects o
         WHERE o.artifact_id = a.id AND o.kind = 'original' AND o.storage_key IS NOT NULL
         ORDER BY o.created_at DESC LIMIT 1),
        'artifacts/' || a.id::text),
    mime_type = COALESCE(a.canonical_mime, a.claimed_mime, 'application/octet-stream'),
    state = CASE WHEN a.storage_state = 'deleted' THEN 'deleted'
                 WHEN a.storage_state = 'deleting' THEN 'deleting'
                 ELSE 'pending' END,
    available_at = CASE WHEN a.storage_state IN ('deleting', 'deleted') THEN COALESCE(a.quarantined_at, a.created_at) END,
    source_type = CASE a.source_type
                      WHEN 'upload'  THEN 'user_upload'
                      WHEN 'sandbox' THEN 'sandbox_publish'
                      WHEN 'tool'    THEN 'tool_output'
                      ELSE 'workflow' END;
UPDATE artifact.artifacts SET size_bytes = NULL, digest = NULL WHERE state = 'pending';
UPDATE artifact.artifacts SET size_bytes = COALESCE(size_bytes, 0),
    digest = COALESCE(digest, 'sha256:' || repeat('0', 64)) WHERE state <> 'pending';

DROP TABLE artifact.search_documents;
DROP TABLE artifact.events;
DROP TABLE artifact.security_attempts;
DROP FUNCTION artifact.events_append_only();
DROP FUNCTION artifact.security_attempt_immutable();
DROP TABLE artifact.objects;

DROP INDEX artifact.artifacts_idempotency;
DROP INDEX artifact.artifacts_child_order;
DROP INDEX artifact.artifacts_storage_state;
DROP INDEX artifact.artifacts_security_pending;
DROP INDEX artifact.artifacts_recent;
DROP INDEX artifact.artifacts_retention;

ALTER TABLE artifact.artifacts
    DROP CONSTRAINT artifacts_upload_id_unique,
    DROP CONSTRAINT artifacts_source_type_check,
    DROP CONSTRAINT artifacts_authorization_source_check,
    DROP CONSTRAINT artifacts_storage_state_check,
    DROP CONSTRAINT artifacts_security_state_check,
    DROP CONSTRAINT artifacts_extraction_state_check,
    DROP CONSTRAINT artifacts_child_shape,
    DROP CONSTRAINT artifacts_container_depth_check,
    DROP CONSTRAINT artifacts_integrity_shape,
    DROP CONSTRAINT artifacts_size_limit,
    DROP CONSTRAINT artifacts_cleared_shape,
    DROP CONSTRAINT artifacts_registration_shape,
    DROP CONSTRAINT artifacts_deleted_shape,
    DROP CONSTRAINT artifacts_deleting_shape,
    DROP CONSTRAINT artifacts_live_name;

ALTER TABLE artifact.artifacts
    DROP COLUMN upload_id,
    DROP COLUMN idempotency_key,
    DROP COLUMN idempotency_fingerprint,
    DROP COLUMN parent_artifact_id,
    DROP COLUMN child_index,
    DROP COLUMN child_kind,
    DROP COLUMN container_depth,
    DROP COLUMN initiating_human_identity_id,
    DROP COLUMN executing_identity_id,
    DROP COLUMN authorization_source,
    DROP COLUMN session_id,
    DROP COLUMN api_key_id,
    DROP COLUMN display_name,
    DROP COLUMN claimed_mime,
    DROP COLUMN canonical_mime,
    DROP COLUMN storage_state,
    DROP COLUMN security_state,
    DROP COLUMN security_reason,
    DROP COLUMN extraction_state,
    DROP COLUMN registered_at,
    DROP COLUMN withdrawn_at,
    DROP COLUMN withdrawn_reason,
    DROP COLUMN quarantined_at,
    DROP COLUMN cleared_at,
    DROP COLUMN deletion_actor_identity_id,
    DROP COLUMN deletion_api_key_id,
    DROP COLUMN row_version;

ALTER TABLE artifact.artifacts
    ALTER COLUMN storage_key SET NOT NULL,
    ALTER COLUMN mime_type SET NOT NULL,
    ALTER COLUMN state SET NOT NULL,
    ALTER COLUMN state SET DEFAULT 'pending',
    ADD CONSTRAINT artifacts_storage_key_key UNIQUE (storage_key),
    ADD CONSTRAINT artifacts_source_type_check CHECK (source_type IN
        ('user_upload', 'sandbox_publish', 'tool_output', 'workflow')),
    ADD CONSTRAINT artifacts_state_check CHECK (state IN ('pending', 'available', 'deleting', 'deleted')),
    ADD CONSTRAINT artifacts_check CHECK ((state = 'pending' AND size_bytes IS NULL AND digest IS NULL AND available_at IS NULL)
        OR (state IN ('available', 'deleting', 'deleted') AND size_bytes IS NOT NULL
            AND digest IS NOT NULL AND available_at IS NOT NULL)),
    ADD CONSTRAINT artifacts_check1 CHECK ((state = 'deleted' AND deleted_at IS NOT NULL)
        OR (state <> 'deleted' AND deleted_at IS NULL)),
    ADD CONSTRAINT artifacts_check2 CHECK ((state IN ('deleting', 'deleted') AND deletion_started_at IS NOT NULL)
        OR (state IN ('pending', 'available') AND deletion_started_at IS NULL));

CREATE INDEX idx_artifacts_state_created ON artifact.artifacts (state, created_at);
CREATE INDEX idx_artifacts_retention ON artifact.artifacts (retention_until)
    WHERE state = 'available' AND retention_until IS NOT NULL;

CREATE FUNCTION artifact.enforce_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id <> OLD.id
       OR NEW.storage_key <> OLD.storage_key
       OR NEW.source_type <> OLD.source_type
       OR NEW.source_ref IS DISTINCT FROM OLD.source_ref
       OR NEW.created_by_identity_id <> OLD.created_by_identity_id
       OR NEW.mime_type <> OLD.mime_type
       OR NEW.retention_until IS DISTINCT FROM OLD.retention_until
       OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'artifact immutable metadata cannot be changed';
    END IF;
    IF OLD.state = 'pending' AND NEW.state <> 'available' THEN
        RAISE EXCEPTION 'pending artifact may transition only to available';
    ELSIF OLD.state = 'available' AND NEW.state <> 'deleting' THEN
        RAISE EXCEPTION 'available artifact may transition only to deleting';
    ELSIF OLD.state = 'deleting' AND NEW.state NOT IN ('deleting', 'deleted') THEN
        RAISE EXCEPTION 'deleting artifact may remain deleting or transition to deleted';
    ELSIF OLD.state = 'deleted' THEN
        RAISE EXCEPTION 'deleted artifact tombstone is immutable';
    END IF;
    IF OLD.state <> 'pending' AND
       (NEW.size_bytes IS DISTINCT FROM OLD.size_bytes OR NEW.digest IS DISTINCT FROM OLD.digest
        OR NEW.available_at IS DISTINCT FROM OLD.available_at) THEN
        RAISE EXCEPTION 'artifact integrity metadata cannot be changed after availability';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER artifact_immutable BEFORE UPDATE ON artifact.artifacts
    FOR EACH ROW EXECUTE FUNCTION artifact.enforce_immutable();
