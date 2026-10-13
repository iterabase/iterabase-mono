-- HOR-670: reverse the language preference. People following the
-- organisation default take its current value (or English) as an explicit
-- choice, so no account loses its effective language.
UPDATE identity.local_users
SET locale = COALESCE(
    (SELECT default_locale FROM identity.organization_settings WHERE id),
    'en')
WHERE locale IS NULL;

DROP TABLE identity.organization_settings;

ALTER TABLE identity.local_users
    ALTER COLUMN locale SET DEFAULT 'en',
    ALTER COLUMN locale SET NOT NULL;
