-- HOR-459 (DES-HOR-459-08): the installation name. Empty means unset; the
-- control-plane API seeds it once from the chart value when it is still
-- empty, and only an Admin changes it afterwards. Trimming and the 80-character
-- limit are validated in Go.
ALTER TABLE identity.organization_settings
    ADD COLUMN name text NOT NULL DEFAULT '';
