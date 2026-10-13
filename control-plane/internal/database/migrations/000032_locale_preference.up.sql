-- HOR-670 (DES-HOR-670-02): language preference and organisation default.
--
-- A person's locale becomes optional: NULL means "follow the organisation
-- default". Existing values were chosen explicitly at access request, setup or
-- in Profile, so they are kept. New accounts that never chose (for example an
-- Admin-created person) start following the organisation.
ALTER TABLE identity.local_users
    ALTER COLUMN locale DROP NOT NULL,
    ALTER COLUMN locale DROP DEFAULT;

-- One deployment is one customer organisation, so its settings are a single
-- row. default_locale stays NULL until an Admin chooses it; resolution then
-- falls back to the browser (web requests) or English (server-side).
CREATE TABLE identity.organization_settings (
    id                     boolean PRIMARY KEY DEFAULT true CHECK (id),
    default_locale         text CHECK (default_locale IN ('en', 'pt')),
    updated_by_identity_id uuid REFERENCES identity.identities(id),
    updated_at             timestamptz NOT NULL DEFAULT now()
);

INSERT INTO identity.organization_settings (id) VALUES (true);

CREATE TRIGGER organization_settings_updated_at BEFORE UPDATE ON identity.organization_settings
    FOR EACH ROW EXECUTE FUNCTION identity.set_updated_at();
