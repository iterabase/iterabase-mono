-- Central inference gateway store (HOR-661; DES-HOR-661-02/04/05/06/07).
-- Payload-free by construction: no column holds prompt, response or provider
-- error content.

CREATE TABLE central.ca (
    id              boolean PRIMARY KEY DEFAULT true CHECK (id),
    certificate_pem text NOT NULL,
    kms_key_id      text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE central.installations (
    id                    text PRIMARY KEY CHECK (id ~ '^[a-z][a-z0-9-]{1,61}[a-z0-9]$'),
    deployment            text NOT NULL CHECK (deployment IN ('hosted', 'self_hosted_managed', 'internal')),
    plan                  text NOT NULL CHECK (plan IN ('trial', 'starter', 'growth', 'scale', 'none')),
    billing_currency      text NOT NULL CHECK (billing_currency IN ('EUR', 'USD')),
    registered_trial_days integer CHECK (registered_trial_days > 0),
    trial_started_at      timestamptz,
    trial_ends_at         timestamptz,
    trial_credits         numeric(20, 6),
    allowance_credits     numeric(20, 6) CHECK (allowance_credits >= 0),
    status                text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked')),
    revoked_at            timestamptz,
    entitlements_version  bigint NOT NULL DEFAULT 1,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CHECK ((deployment = 'hosted') = (plan <> 'none')),
    CHECK (plan <> 'trial' OR (trial_started_at IS NOT NULL AND trial_ends_at IS NOT NULL AND trial_credits IS NOT NULL))
);

CREATE TABLE central.certificates (
    serial             text PRIMARY KEY,
    installation_id    text NOT NULL REFERENCES central.installations (id),
    fingerprint_sha256 text NOT NULL UNIQUE,
    not_before         timestamptz NOT NULL,
    not_after          timestamptz NOT NULL,
    issued_at          timestamptz NOT NULL DEFAULT now(),
    revoked_at         timestamptz
);
CREATE INDEX certificates_installation ON central.certificates (installation_id);

CREATE TABLE central.audit_events (
    id              bigserial PRIMARY KEY,
    at              timestamptz NOT NULL DEFAULT now(),
    actor_arn       text NOT NULL,
    actor_session   text NOT NULL DEFAULT '',
    installation_id text,
    action          text NOT NULL,
    detail          jsonb NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX audit_events_installation ON central.audit_events (installation_id, at);

-- ECB reference observations (USD per 1 EUR) and the per-month locked rates.
CREATE TABLE central.fx_observations (
    observation_date date NOT NULL,
    quote            text NOT NULL CHECK (quote = 'USD'),
    rate             numeric(20, 10) NOT NULL CHECK (rate > 0),
    fetched_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (observation_date, quote)
);

CREATE TABLE central.fx_rates (
    version          bigserial PRIMARY KEY,
    -- The UTC date a billing period starts: the 1st for a month, the trial's
    -- start date for a trial (DES-HOR-661-04/-05).
    lock_date        date NOT NULL,
    quote            text NOT NULL CHECK (quote = 'USD'),
    rate             numeric(20, 10) NOT NULL CHECK (rate > 0),
    observation_date date NOT NULL CHECK (observation_date < lock_date),
    locked_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (lock_date, quote)
);

CREATE TABLE central.usage_records (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id          text NOT NULL,
    installation_id     text NOT NULL REFERENCES central.installations (id),
    occurred_at         timestamptz NOT NULL,
    period_start        timestamptz NOT NULL,
    model               text NOT NULL,
    provider_model      text NOT NULL,
    stream              boolean NOT NULL,
    disposition         text NOT NULL CHECK (disposition IN ('completed', 'failed', 'canceled', 'outcome_unknown')),
    error_code          text NOT NULL DEFAULT '',
    upstream_status     integer,
    input_tokens        integer NOT NULL DEFAULT 0,
    cached_input_tokens integer NOT NULL DEFAULT 0,
    output_tokens       integer NOT NULL DEFAULT 0,
    supplier_currency   text NOT NULL CHECK (supplier_currency IN ('EUR', 'USD')),
    supplier_amount     numeric(24, 12) NOT NULL,
    price_version       text NOT NULL,
    fx_version          bigint REFERENCES central.fx_rates (version),
    fx_rate             numeric(20, 10),
    billing_currency    text NOT NULL CHECK (billing_currency IN ('EUR', 'USD')),
    credits             numeric(20, 6) NOT NULL,
    classification      text NOT NULL CHECK (classification IN ('chargeable', 'company_funded', 'not_charged')),
    adjusted_at         timestamptz,
    adjustment_reason   text,
    identity_id         text NOT NULL DEFAULT '',
    run_id              text NOT NULL DEFAULT '',
    turn_id             text NOT NULL DEFAULT '',
    workflow_key        text NOT NULL DEFAULT '',
    work_item_id        text NOT NULL DEFAULT '',
    channel             text NOT NULL DEFAULT ''
);
CREATE INDEX usage_records_period ON central.usage_records (installation_id, period_start);
CREATE INDEX usage_records_occurred ON central.usage_records (occurred_at);

CREATE TABLE central.period_usage (
    installation_id        text NOT NULL REFERENCES central.installations (id),
    period_start           timestamptz NOT NULL,
    period_end             timestamptz NOT NULL,
    period_kind            text NOT NULL CHECK (period_kind IN ('month', 'trial')),
    allowance_credits      numeric(20, 6) NOT NULL DEFAULT 0,
    chargeable_credits     numeric(20, 6) NOT NULL DEFAULT 0,
    company_funded_credits numeric(20, 6) NOT NULL DEFAULT 0,
    requests               bigint NOT NULL DEFAULT 0,
    not_charged_requests   bigint NOT NULL DEFAULT 0,
    updated_at             timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (installation_id, period_start)
);

CREATE TABLE central.alerts (
    installation_id   text NOT NULL REFERENCES central.installations (id),
    period_start      timestamptz NOT NULL,
    threshold         integer NOT NULL CHECK (threshold IN (80, 100)),
    crossed_at        timestamptz NOT NULL,
    used_credits      numeric(20, 6) NOT NULL,
    allowance_credits numeric(20, 6) NOT NULL,
    notified_at       timestamptz,
    PRIMARY KEY (installation_id, period_start, threshold)
);

CREATE TABLE central.rate_counters (
    scope        text NOT NULL,
    window_start timestamptz NOT NULL,
    requests     integer NOT NULL DEFAULT 0,
    tokens       bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (scope, window_start)
);

-- Every replica's in-memory registry refreshes on this notification
-- (DES-HOR-661-02: revocation propagates via LISTEN/NOTIFY).
CREATE FUNCTION central.notify_registry_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('central_registry_changed', '');
    RETURN NULL;
END $$;

CREATE TRIGGER installations_changed AFTER INSERT OR UPDATE OR DELETE ON central.installations
    FOR EACH STATEMENT EXECUTE FUNCTION central.notify_registry_changed();
CREATE TRIGGER certificates_changed AFTER INSERT OR UPDATE OR DELETE ON central.certificates
    FOR EACH STATEMENT EXECUTE FUNCTION central.notify_registry_changed();
