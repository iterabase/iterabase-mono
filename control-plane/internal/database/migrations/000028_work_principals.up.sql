-- HOR-454: persist the approved three-principal runtime seam (architecture 7.15,
-- DES-HOR-451-05).
--
-- New customer-originated work records the initiating human, the request actor,
-- and the existing executing workflow/agent scope identity as separate durable
-- principals, plus the authorization evidence that produced the request. The
-- columns are nullable so every pre-epoch row stays valid immutable history;
-- callers can never supply a trusted principal UUID directly.

ALTER TABLE work.work_items
    ADD COLUMN initiating_human_identity_id uuid REFERENCES identity.identities(id),
    ADD COLUMN request_actor_identity_id    uuid REFERENCES identity.identities(id),
    ADD COLUMN authorization_source         text CHECK (authorization_source IN ('browser', 'api', 'chat')),
    ADD COLUMN authorization_api_key_id     uuid,
    ADD COLUMN authorization_session_id     uuid;

CREATE INDEX work_items_initiating_human
    ON work.work_items (initiating_human_identity_id)
    WHERE initiating_human_identity_id IS NOT NULL;
CREATE INDEX work_items_request_actor
    ON work.work_items (request_actor_identity_id)
    WHERE request_actor_identity_id IS NOT NULL;

ALTER TABLE runtime.workflow_runs
    ADD COLUMN initiating_human_identity_id uuid,
    ADD COLUMN request_actor_identity_id    uuid,
    ADD COLUMN authorization_source         text CHECK (authorization_source IN ('browser', 'api', 'chat')),
    ADD COLUMN authorization_api_key_id     uuid,
    ADD COLUMN authorization_session_id     uuid;
