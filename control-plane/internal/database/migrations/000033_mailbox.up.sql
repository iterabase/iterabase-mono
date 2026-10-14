-- HOR-358: provider-neutral mailbox sources, intake state, and the event inbox
-- (DES-HOR-358-02/03).
--
-- `mailbox.sources` is the authoritative mailbox list. Intake (the tool gateway
-- process), the gateway mailbox scope check, and mailbox start binding
-- (HOR-595) read it. Every row has exactly one owner (DES-HOR-593-03): the
-- overlay `Mailbox` CRD writes `owner = 'overlay'` rows; installation-owned
-- rows (HOR-674) are never overwritten or pruned by the reconciler.
--
-- No credential value lives here. A source names a credential slot; the
-- gateway resolves the slot to its installation-owned Kubernetes Secret.

CREATE SCHEMA IF NOT EXISTS mailbox;

CREATE TABLE mailbox.sources (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name                   text NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    owner                  text NOT NULL CHECK (owner IN ('overlay', 'installation')),
    -- Overlay rows record the declaring object (`<namespace>/<name>`).
    owner_ref              text NOT NULL DEFAULT '',
    provider               text NOT NULL CHECK (provider IN ('microsoft365')),
    -- Lower-cased, plain address; the provider's mailbox identity.
    address                text NOT NULL CHECK (address = lower(address) AND length(address) BETWEEN 3 AND 320),
    folders                text[] NOT NULL CHECK (cardinality(folders) BETWEEN 1 AND 16),
    credential_slot        text NOT NULL CHECK (credential_slot ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    -- Empty means polling-only: no provider subscription is created.
    notification_url       text NOT NULL DEFAULT '' CHECK (notification_url = '' OR notification_url ~ '^https://'),
    poll_interval_seconds  integer NOT NULL DEFAULT 60 CHECK (poll_interval_seconds BETWEEN 10 AND 3600),
    -- Next intake run; the loop claims due rows under a lease.
    next_poll_at           timestamptz NOT NULL DEFAULT now(),
    lease_until            timestamptz,
    last_polled_at         timestamptz,
    disabled_at            timestamptz,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now()
);
-- One active mailbox per name and per provider address, across both owners.
CREATE UNIQUE INDEX sources_active_name ON mailbox.sources (name) WHERE disabled_at IS NULL;
CREATE UNIQUE INDEX sources_active_address ON mailbox.sources (provider, address) WHERE disabled_at IS NULL;
CREATE INDEX sources_due ON mailbox.sources (next_poll_at) WHERE disabled_at IS NULL;
CREATE TRIGGER sources_updated_at BEFORE UPDATE ON mailbox.sources
    FOR EACH ROW EXECUTE FUNCTION identity.set_updated_at();

-- The provider delta position per source folder. `baseline_at` is when the
-- folder was first observed: mail received before it is history, never an
-- event. A reset cursor resumes from the newest event already recorded.
CREATE TABLE mailbox.folder_cursors (
    source_id    uuid NOT NULL REFERENCES mailbox.sources(id) ON DELETE CASCADE,
    folder       text NOT NULL CHECK (length(folder) BETWEEN 1 AND 255),
    cursor       text NOT NULL DEFAULT '',
    baseline_at  timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (source_id, folder)
);

-- Provider change-notification subscriptions per source folder. Only a SHA-256
-- of the clientState is kept; the HOR-356 receiver hashes what it is sent.
CREATE TABLE mailbox.subscriptions (
    source_id                 uuid NOT NULL REFERENCES mailbox.sources(id) ON DELETE CASCADE,
    folder                    text NOT NULL,
    provider_subscription_id  text NOT NULL,
    notification_url          text NOT NULL,
    client_state_sha256       text NOT NULL CHECK (client_state_sha256 ~ '^[0-9a-f]{64}$'),
    expires_at                timestamptz NOT NULL,
    created_at                timestamptz NOT NULL DEFAULT now(),
    renewed_at                timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (source_id, folder),
    UNIQUE (provider_subscription_id)
);

-- The provider-neutral event inbox: one row per new message, ever. The unique
-- provider identity is the deduplication key, so delta replays and later
-- notifications never produce a second event. No body is stored; consumers
-- read content through the bounded product tools.
CREATE TABLE mailbox.messages (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id             uuid NOT NULL REFERENCES mailbox.sources(id) ON DELETE RESTRICT,
    provider_message_id   text NOT NULL CHECK (length(provider_message_id) BETWEEN 1 AND 1024),
    folder                text NOT NULL,
    internet_message_id   text NOT NULL DEFAULT '',
    conversation_id       text NOT NULL DEFAULT '',
    in_reply_to           text NOT NULL DEFAULT '',
    "references"          text[] NOT NULL DEFAULT '{}',
    sender_address        text NOT NULL DEFAULT '',
    sender_name           text NOT NULL DEFAULT '',
    subject               text NOT NULL DEFAULT '',
    has_attachments       boolean NOT NULL DEFAULT false,
    received_at           timestamptz NOT NULL,
    recorded_at           timestamptz NOT NULL DEFAULT now(),
    -- Single-consumer claim/acknowledge (mailbox routing, HOR-595).
    claimed_until         timestamptz,
    acknowledged_at       timestamptz,
    UNIQUE (source_id, provider_message_id)
);
CREATE INDEX messages_pending ON mailbox.messages (recorded_at) WHERE acknowledged_at IS NULL;
CREATE INDEX messages_internet_message_id ON mailbox.messages (internet_message_id) WHERE internet_message_id <> '';
CREATE INDEX messages_conversation ON mailbox.messages (source_id, conversation_id) WHERE conversation_id <> '';

-- The latest credential outcome per slot, with a customer-safe reason. HOR-652
-- shows it as the slot's "needs attention" state. Never holds a value.
CREATE TABLE mailbox.credential_status (
    credential_slot  text PRIMARY KEY,
    state            text NOT NULL CHECK (state IN ('ok', 'failing')),
    reason           text NOT NULL DEFAULT '' CHECK (length(reason) <= 512),
    observed_at      timestamptz NOT NULL DEFAULT now()
);
