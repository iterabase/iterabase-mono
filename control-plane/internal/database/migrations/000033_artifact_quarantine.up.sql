-- HOR-461: V2 artifact quarantine, shared library, and controlled lifecycle
-- (docs/architecture/v2-artifact-processing.md, DES-HOR-460-01..12 and
-- DES-HOR-461-01..04).
--
-- The v1 single `state` (pending -> available -> deleting -> deleted) is
-- replaced by independent storage, security, extraction, and registration
-- state. Dropping `state` is the epoch boundary: an older binary that would
-- read `available` as safe fails on its first artifact query instead of
-- serving unscanned bytes. Every legacy row is reclassified as unscanned
-- quarantine and re-enters the normal scan path; nothing is grandfathered to
-- `cleared`.
--
-- The transition is one-way: rollback is roll-forward or catastrophic restore.

-- ---------------------------------------------------------------------------
-- Artifact identity, attribution, and independent state
-- ---------------------------------------------------------------------------

DROP TRIGGER artifact_immutable ON artifact.artifacts;
DROP FUNCTION artifact.enforce_immutable();
DROP INDEX artifact.idx_artifacts_state_created;
DROP INDEX artifact.idx_artifacts_retention;

ALTER TABLE artifact.artifacts DROP CONSTRAINT artifacts_check;
ALTER TABLE artifact.artifacts DROP CONSTRAINT artifacts_check1;
ALTER TABLE artifact.artifacts DROP CONSTRAINT artifacts_check2;
ALTER TABLE artifact.artifacts DROP CONSTRAINT artifacts_source_type_check;

ALTER TABLE artifact.artifacts
    ADD COLUMN upload_id                    uuid NOT NULL DEFAULT gen_random_uuid(),
    ADD COLUMN idempotency_key              text,
    ADD COLUMN idempotency_fingerprint      text,
    ADD COLUMN parent_artifact_id           uuid REFERENCES artifact.artifacts(id) ON DELETE RESTRICT,
    ADD COLUMN child_index                  integer,
    ADD COLUMN child_kind                   text,
    ADD COLUMN container_depth              smallint NOT NULL DEFAULT 1,
    ADD COLUMN initiating_human_identity_id uuid REFERENCES identity.identities(id) ON DELETE RESTRICT,
    ADD COLUMN executing_identity_id        uuid REFERENCES identity.identities(id) ON DELETE RESTRICT,
    ADD COLUMN authorization_source         text,
    ADD COLUMN session_id                   uuid,
    ADD COLUMN api_key_id                   uuid,
    ADD COLUMN display_name                 text,
    ADD COLUMN claimed_mime                 text,
    ADD COLUMN canonical_mime               text,
    ADD COLUMN storage_state                text,
    ADD COLUMN security_state               text,
    ADD COLUMN security_reason              text,
    ADD COLUMN extraction_state             text NOT NULL DEFAULT 'not_eligible',
    ADD COLUMN registered_at                timestamptz,
    ADD COLUMN withdrawn_at                 timestamptz,
    ADD COLUMN withdrawn_reason             text,
    ADD COLUMN quarantined_at               timestamptz,
    ADD COLUMN cleared_at                   timestamptz,
    ADD COLUMN deletion_actor_identity_id   uuid REFERENCES identity.identities(id) ON DELETE RESTRICT,
    ADD COLUMN deletion_api_key_id          uuid,
    ADD COLUMN row_version                  bigint NOT NULL DEFAULT 0;

-- Legacy rows: every committed object becomes unscanned quarantine. A legacy
-- pending upload never committed and is swept as an incomplete upload.
UPDATE artifact.artifacts SET
    claimed_mime   = mime_type,
    display_name   = CASE WHEN state = 'deleted' THEN NULL ELSE 'artifact-' || id::text END,
    source_type    = CASE source_type
                         WHEN 'user_upload'     THEN 'upload'
                         WHEN 'sandbox_publish' THEN 'sandbox'
                         WHEN 'tool_output'     THEN 'tool'
                         ELSE source_type END,
    storage_state  = CASE state
                         WHEN 'pending'   THEN 'uploading'
                         WHEN 'available' THEN 'quarantined'
                         ELSE state END,
    security_state = CASE state WHEN 'deleted' THEN 'rejected' ELSE 'pending' END,
    security_reason = CASE state WHEN 'deleted' THEN 'legacy_deleted' ELSE NULL END,
    quarantined_at = CASE WHEN state IN ('available', 'deleting', 'deleted') THEN available_at END;

ALTER TABLE artifact.artifacts DROP COLUMN state;
ALTER TABLE artifact.artifacts DROP COLUMN available_at;
ALTER TABLE artifact.artifacts DROP COLUMN mime_type;
-- Object keys move to artifact.objects; one artifact has several objects.
ALTER TABLE artifact.artifacts RENAME COLUMN storage_key TO legacy_storage_key;
ALTER TABLE artifact.artifacts ALTER COLUMN legacy_storage_key DROP NOT NULL;

ALTER TABLE artifact.artifacts
    ALTER COLUMN storage_state SET NOT NULL,
    ALTER COLUMN storage_state SET DEFAULT 'uploading',
    ALTER COLUMN security_state SET NOT NULL,
    ALTER COLUMN security_state SET DEFAULT 'pending';

ALTER TABLE artifact.artifacts
    ADD CONSTRAINT artifacts_upload_id_unique UNIQUE (upload_id),
    ADD CONSTRAINT artifacts_source_type_check CHECK (source_type IN
        ('upload', 'chat', 'teams', 'mailbox', 'runner', 'tool', 'workflow', 'sandbox')),
    ADD CONSTRAINT artifacts_authorization_source_check CHECK (authorization_source IS NULL
        OR authorization_source IN ('browser', 'api', 'chat', 'workload', 'system')),
    ADD CONSTRAINT artifacts_storage_state_check CHECK (storage_state IN
        ('uploading', 'quarantined', 'promoting', 'stored', 'deleting', 'deleted')),
    ADD CONSTRAINT artifacts_security_state_check CHECK (security_state IN
        ('pending', 'scanning', 'cleared', 'rejected', 'retryable_failed', 'rescan_pending')),
    ADD CONSTRAINT artifacts_extraction_state_check CHECK (extraction_state IN
        ('not_eligible', 'pending', 'extracting', 'ready', 'retryable_failed', 'failed')),
    ADD CONSTRAINT artifacts_child_shape CHECK (
        (parent_artifact_id IS NULL AND child_index IS NULL AND child_kind IS NULL)
        OR (parent_artifact_id IS NOT NULL AND child_index >= 0
            AND child_kind IN ('body', 'attachment'))),
    ADD CONSTRAINT artifacts_container_depth_check CHECK (container_depth BETWEEN 0 AND 16),
    -- Integrity is established exactly when the upload stream commits.
    ADD CONSTRAINT artifacts_integrity_shape CHECK (
        (storage_state = 'uploading' AND size_bytes IS NULL AND digest IS NULL)
        OR (storage_state <> 'uploading' AND size_bytes IS NOT NULL AND digest IS NOT NULL)
        OR (storage_state = 'deleted' AND size_bytes IS NULL AND digest IS NULL)),
    ADD CONSTRAINT artifacts_size_limit CHECK (size_bytes IS NULL OR size_bytes <= 26214400),
    -- Clearance is meaningful only for stored bytes with a canonical type.
    ADD CONSTRAINT artifacts_cleared_shape CHECK (
        security_state <> 'cleared'
        OR (storage_state IN ('stored', 'deleting', 'deleted') AND canonical_mime IS NOT NULL
            AND cleared_at IS NOT NULL)),
    -- First registration happens only in the clearance transaction (and is
    -- immutable); current visibility is withdrawn_at plus artifact.is_cleared,
    -- so a registered artifact may be re-scanned through any security state.
    ADD CONSTRAINT artifacts_registration_shape CHECK (
        registered_at IS NULL OR storage_state IN ('stored', 'deleting', 'deleted')),
    ADD CONSTRAINT artifacts_deleted_shape CHECK (
        (storage_state = 'deleted' AND deleted_at IS NOT NULL AND display_name IS NULL)
        OR (storage_state <> 'deleted' AND deleted_at IS NULL)),
    ADD CONSTRAINT artifacts_deleting_shape CHECK (
        (storage_state IN ('deleting', 'deleted') AND deletion_started_at IS NOT NULL)
        OR (storage_state NOT IN ('deleting', 'deleted') AND deletion_started_at IS NULL)),
    ADD CONSTRAINT artifacts_live_name CHECK (storage_state = 'deleted' OR display_name IS NOT NULL);

-- An idempotency key is scoped to the request actor that supplied it.
CREATE UNIQUE INDEX artifacts_idempotency ON artifact.artifacts (created_by_identity_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
-- One live child per container position. An abandoned (deleted) reservation
-- from an interrupted expansion never blocks the retry.
CREATE UNIQUE INDEX artifacts_child_order ON artifact.artifacts (parent_artifact_id, child_index)
    WHERE parent_artifact_id IS NOT NULL AND storage_state <> 'deleted';
CREATE INDEX artifacts_storage_state ON artifact.artifacts (storage_state, created_at);
CREATE INDEX artifacts_security_pending ON artifact.artifacts (quarantined_at)
    WHERE storage_state = 'quarantined';
CREATE INDEX artifacts_recent ON artifact.artifacts (registered_at DESC, id DESC)
    WHERE registered_at IS NOT NULL AND withdrawn_at IS NULL;
CREATE INDEX artifacts_retention ON artifact.artifacts (retention_until)
    WHERE storage_state = 'stored' AND retention_until IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Clearance predicates: the database authority every consumer joins on
-- ---------------------------------------------------------------------------

-- Bytes may be used (downloaded, linked to work, materialized, read by a tool)
-- only while this holds immediately before the use (invariant 3).
CREATE FUNCTION artifact.is_cleared(artifact_id uuid) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT EXISTS (
        SELECT 1 FROM artifact.artifacts a
        WHERE a.id = artifact_id
          AND a.storage_state = 'stored'
          AND a.security_state = 'cleared'
          AND a.registered_at IS NOT NULL
          AND a.withdrawn_at IS NULL)
$$;

-- DES-HOR-461-02: a workload may cite its own output while it is still being
-- checked. Rejected, failed, deleting, and deleted outputs are never accepted.
CREATE FUNCTION artifact.is_admissible_output(artifact_id uuid) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT EXISTS (
        SELECT 1 FROM artifact.artifacts a
        WHERE a.id = artifact_id
          AND a.storage_state IN ('quarantined', 'promoting', 'stored')
          AND a.security_state IN ('pending', 'scanning', 'cleared'))
$$;

-- ---------------------------------------------------------------------------
-- Physical objects
-- ---------------------------------------------------------------------------

CREATE TABLE artifact.objects (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    artifact_id   uuid NOT NULL REFERENCES artifact.artifacts(id) ON DELETE RESTRICT,
    zone          text NOT NULL CHECK (zone IN ('quarantine', 'original', 'derived')),
    kind          text NOT NULL CHECK (kind IN ('original', 'html_source', 'text', 'tables', 'page_image')),
    storage_key   text UNIQUE,
    digest        text CHECK (digest IS NULL OR digest ~ '^sha256:[0-9a-f]{64}$'),
    size_bytes    bigint CHECK (size_bytes IS NULL OR size_bytes >= 0),
    content_type  text,
    state         text NOT NULL DEFAULT 'writing' CHECK (state IN ('writing', 'committed', 'purging', 'purged')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    committed_at  timestamptz,
    purged_at     timestamptz,
    purge_error   text,
    CHECK ((state = 'purged' AND purged_at IS NOT NULL AND storage_key IS NULL)
        OR (state <> 'purged' AND storage_key IS NOT NULL)),
    CHECK ((state = 'committed' AND digest IS NOT NULL AND size_bytes IS NOT NULL AND committed_at IS NOT NULL)
        OR state <> 'committed')
);
CREATE INDEX objects_artifact ON artifact.objects (artifact_id, zone, kind);
CREATE INDEX objects_unpurged ON artifact.objects (state, created_at) WHERE state <> 'purged';

-- Every legacy object becomes the committed quarantine original of its row.
INSERT INTO artifact.objects (artifact_id, zone, kind, storage_key, digest, size_bytes, content_type, state, committed_at)
SELECT id, 'quarantine', 'original', legacy_storage_key, digest, size_bytes, claimed_mime, 'committed', quarantined_at
FROM artifact.artifacts WHERE storage_state = 'quarantined';
INSERT INTO artifact.objects (artifact_id, zone, kind, storage_key, digest, size_bytes, state, committed_at)
SELECT id, 'quarantine', 'original', legacy_storage_key, digest, size_bytes,
       CASE WHEN storage_state = 'deleting' THEN 'committed' ELSE 'writing' END,
       CASE WHEN storage_state = 'deleting' THEN deletion_started_at END
FROM artifact.artifacts WHERE storage_state IN ('uploading', 'deleting');
UPDATE artifact.artifacts SET legacy_storage_key = NULL;
ALTER TABLE artifact.artifacts DROP COLUMN legacy_storage_key;

-- ---------------------------------------------------------------------------
-- Security attempts (append-only evidence, fenced leases)
-- ---------------------------------------------------------------------------

CREATE TABLE artifact.security_attempts (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    artifact_id            uuid NOT NULL REFERENCES artifact.artifacts(id) ON DELETE RESTRICT,
    attempt_number         integer NOT NULL CHECK (attempt_number >= 1),
    trigger_id             uuid NOT NULL,
    trigger_attempt        smallint NOT NULL CHECK (trigger_attempt BETWEEN 1 AND 3),
    trigger_kind           text NOT NULL CHECK (trigger_kind IN
                               ('initial', 'automatic_retry', 'manual_retry', 'security_advisory')),
    trigger_actor_identity_id uuid REFERENCES identity.identities(id) ON DELETE RESTRICT,
    trigger_session_id     uuid,
    state                  text NOT NULL DEFAULT 'queued' CHECK (state IN
                               ('queued', 'leased', 'cleared', 'rejected', 'retryable_failed', 'abandoned')),
    not_before             timestamptz NOT NULL DEFAULT now(),
    lease_owner            text,
    lease_generation       bigint NOT NULL DEFAULT 0,
    lease_expires_at       timestamptz,
    input_object_id        uuid REFERENCES artifact.objects(id) ON DELETE RESTRICT,
    input_digest           text,
    engine_version         text,
    signature_version      text,
    signature_age_seconds  bigint,
    policy_version         text,
    canonical_mime         text,
    outcome_reason         text,
    created_at             timestamptz NOT NULL DEFAULT now(),
    started_at             timestamptz,
    finished_at            timestamptz,
    UNIQUE (artifact_id, attempt_number),
    CHECK ((state IN ('cleared', 'rejected', 'retryable_failed', 'abandoned')) = (finished_at IS NOT NULL)),
    CHECK (state <> 'leased' OR (lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL))
);
CREATE UNIQUE INDEX security_attempts_one_open ON artifact.security_attempts (artifact_id)
    WHERE state IN ('queued', 'leased');
CREATE INDEX security_attempts_due ON artifact.security_attempts (not_before)
    WHERE state = 'queued';
CREATE INDEX security_attempts_leases ON artifact.security_attempts (lease_expires_at)
    WHERE state = 'leased';

-- Terminal attempts are immutable evidence.
CREATE FUNCTION artifact.security_attempt_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.state IN ('cleared', 'rejected', 'retryable_failed', 'abandoned') THEN
        RAISE EXCEPTION 'terminal security attempt is immutable';
    END IF;
    IF NEW.artifact_id <> OLD.artifact_id OR NEW.attempt_number <> OLD.attempt_number
       OR NEW.trigger_id <> OLD.trigger_id OR NEW.trigger_kind <> OLD.trigger_kind THEN
        RAISE EXCEPTION 'security attempt identity is immutable';
    END IF;
    IF NEW.lease_generation < OLD.lease_generation THEN
        RAISE EXCEPTION 'security attempt lease generation cannot decrease';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER security_attempt_immutable BEFORE UPDATE ON artifact.security_attempts
    FOR EACH ROW EXECUTE FUNCTION artifact.security_attempt_immutable();

-- Legacy unscanned rows enter the normal scan path.
INSERT INTO artifact.security_attempts
    (artifact_id, attempt_number, trigger_id, trigger_attempt, trigger_kind, input_object_id, input_digest)
SELECT a.id, 1, gen_random_uuid(), 1, 'initial', o.id, a.digest
FROM artifact.artifacts a
JOIN artifact.objects o ON o.artifact_id = a.id AND o.zone = 'quarantine' AND o.state = 'committed'
WHERE a.storage_state = 'quarantined';

-- ---------------------------------------------------------------------------
-- Lifecycle events (append-only, content-free)
-- ---------------------------------------------------------------------------

CREATE TABLE artifact.events (
    id                bigserial PRIMARY KEY,
    artifact_id       uuid NOT NULL REFERENCES artifact.artifacts(id) ON DELETE RESTRICT,
    event             text NOT NULL,
    actor_identity_id uuid REFERENCES identity.identities(id) ON DELETE RESTRICT,
    reason            text,
    detail            jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX events_artifact ON artifact.events (artifact_id, id);

CREATE FUNCTION artifact.events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'artifact events are append-only';
END;
$$;
CREATE TRIGGER events_append_only BEFORE UPDATE OR DELETE ON artifact.events
    FOR EACH ROW EXECUTE FUNCTION artifact.events_append_only();
CREATE TRIGGER security_attempts_append_only BEFORE DELETE ON artifact.security_attempts
    FOR EACH ROW EXECUTE FUNCTION artifact.events_append_only();

INSERT INTO artifact.events (artifact_id, event, reason)
SELECT id, 'legacy_reclassified', 'v2_artifact_epoch' FROM artifact.artifacts;

-- ---------------------------------------------------------------------------
-- Installation-wide search projection (registered rows only)
-- ---------------------------------------------------------------------------

CREATE TABLE artifact.search_documents (
    artifact_id     uuid PRIMARY KEY REFERENCES artifact.artifacts(id) ON DELETE RESTRICT,
    display_name    text NOT NULL,
    canonical_mime  text NOT NULL,
    source_label    text NOT NULL,
    uploader_name   text NOT NULL DEFAULT '',
    content_text    text NOT NULL DEFAULT '',
    registered_at   timestamptz NOT NULL,
    -- Filenames are indexed whole and split at punctuation, so "q3-notes.txt"
    -- matches both "q3-notes.txt" and "notes".
    metadata_tsv    tsvector GENERATED ALWAYS AS (to_tsvector('simple',
                        display_name || ' ' || regexp_replace(display_name, '[[:punct:]]+', ' ', 'g') || ' '
                        || canonical_mime || ' ' || source_label || ' ' || uploader_name)) STORED,
    content_tsv     tsvector GENERATED ALWAYS AS (to_tsvector('simple', content_text)) STORED
);
CREATE INDEX search_documents_metadata ON artifact.search_documents USING gin (metadata_tsv);
CREATE INDEX search_documents_content ON artifact.search_documents USING gin (content_tsv);

-- ---------------------------------------------------------------------------
-- Immutability and legal transitions
-- ---------------------------------------------------------------------------

CREATE FUNCTION artifact.enforce_lifecycle() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id <> OLD.id
       OR NEW.upload_id <> OLD.upload_id
       OR NEW.source_type <> OLD.source_type
       OR NEW.source_ref IS DISTINCT FROM OLD.source_ref
       OR NEW.parent_artifact_id IS DISTINCT FROM OLD.parent_artifact_id
       OR NEW.child_index IS DISTINCT FROM OLD.child_index
       OR NEW.child_kind IS DISTINCT FROM OLD.child_kind
       OR NEW.container_depth <> OLD.container_depth
       OR NEW.created_by_identity_id <> OLD.created_by_identity_id
       OR NEW.initiating_human_identity_id IS DISTINCT FROM OLD.initiating_human_identity_id
       OR NEW.executing_identity_id IS DISTINCT FROM OLD.executing_identity_id
       OR NEW.authorization_source IS DISTINCT FROM OLD.authorization_source
       OR NEW.session_id IS DISTINCT FROM OLD.session_id
       OR NEW.api_key_id IS DISTINCT FROM OLD.api_key_id
       OR NEW.claimed_mime IS DISTINCT FROM OLD.claimed_mime
       OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
       OR NEW.idempotency_fingerprint IS DISTINCT FROM OLD.idempotency_fingerprint
       OR NEW.retention_until IS DISTINCT FROM OLD.retention_until
       OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'artifact identity, source, and attribution are immutable';
    END IF;

    -- Storage lifecycle (section 7): no return to quarantine after promotion
    -- and no return from deleting/deleted.
    IF NEW.storage_state <> OLD.storage_state AND NOT (
           (OLD.storage_state = 'uploading'   AND NEW.storage_state IN ('quarantined', 'deleted'))
        OR (OLD.storage_state = 'quarantined' AND NEW.storage_state IN ('promoting', 'stored', 'deleting'))
        OR (OLD.storage_state = 'promoting'   AND NEW.storage_state IN ('stored', 'deleting'))
        OR (OLD.storage_state = 'stored'      AND NEW.storage_state = 'deleting')
        OR (OLD.storage_state = 'deleting'    AND NEW.storage_state = 'deleted')) THEN
        RAISE EXCEPTION 'illegal artifact storage transition % -> %', OLD.storage_state, NEW.storage_state;
    END IF;
    IF OLD.storage_state = 'deleted' THEN
        RAISE EXCEPTION 'deleted artifact tombstone is immutable';
    END IF;

    -- Security lifecycle: rejected is terminal; clearance is withdrawn only
    -- through rescan_pending.
    IF NEW.security_state <> OLD.security_state AND NOT (
           (OLD.security_state = 'pending'          AND NEW.security_state IN ('scanning', 'rejected'))
        OR (OLD.security_state = 'scanning'         AND NEW.security_state IN ('cleared', 'rejected', 'retryable_failed', 'pending'))
        OR (OLD.security_state = 'retryable_failed' AND NEW.security_state IN ('pending', 'rejected'))
        OR (OLD.security_state = 'cleared'          AND NEW.security_state = 'rescan_pending')
        OR (OLD.security_state = 'rescan_pending'   AND NEW.security_state IN ('scanning', 'rejected'))) THEN
        RAISE EXCEPTION 'illegal artifact security transition % -> %', OLD.security_state, NEW.security_state;
    END IF;

    -- Integrity and canonical type are written once.
    IF OLD.storage_state <> 'uploading' AND NEW.storage_state <> 'deleted'
       AND (NEW.size_bytes IS DISTINCT FROM OLD.size_bytes OR NEW.digest IS DISTINCT FROM OLD.digest) THEN
        RAISE EXCEPTION 'artifact integrity metadata cannot change after upload';
    END IF;
    IF OLD.canonical_mime IS NOT NULL AND NEW.canonical_mime IS DISTINCT FROM OLD.canonical_mime
       AND NEW.storage_state <> 'deleted' THEN
        RAISE EXCEPTION 'artifact canonical type cannot change';
    END IF;
    IF OLD.registered_at IS NOT NULL AND NEW.registered_at IS DISTINCT FROM OLD.registered_at THEN
        RAISE EXCEPTION 'first library registration is immutable';
    END IF;

    NEW.row_version := OLD.row_version + 1;
    RETURN NEW;
END;
$$;
CREATE TRIGGER artifact_lifecycle BEFORE UPDATE ON artifact.artifacts
    FOR EACH ROW EXECUTE FUNCTION artifact.enforce_lifecycle();

-- ---------------------------------------------------------------------------
-- Reference acquisition serializes with deletion (section 16)
-- ---------------------------------------------------------------------------
-- Deletion locks the artifact row FOR UPDATE, checks active references, and
-- enters `deleting`. A reference insert that read the artifact before that
-- commit would otherwise wait on the foreign-key lock and then succeed
-- against bytes deletion has already authorized purging. Every link insert
-- takes FOR SHARE on the artifact (which conflicts with deletion's lock) and
-- re-reads the committed row after acquiring it. Security rejection purges
-- regardless of references and leaves the row `rejected`, so a link to a
-- rejected artifact stays legal: consumers fail closed on it.
CREATE FUNCTION work.artifact_link_serialize() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    storage  text;
    security text;
BEGIN
    SELECT a.storage_state, a.security_state INTO storage, security
    FROM artifact.artifacts a WHERE a.id = NEW.artifact_id FOR SHARE;
    IF storage IN ('deleting', 'deleted') AND security <> 'rejected' THEN
        RAISE EXCEPTION 'artifact_unavailable: artifact % is being deleted', NEW.artifact_id
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER artifact_link_serialize BEFORE INSERT ON work.artifact_links
    FOR EACH ROW EXECUTE FUNCTION work.artifact_link_serialize();
