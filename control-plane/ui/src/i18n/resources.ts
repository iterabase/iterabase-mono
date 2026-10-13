// Catalog registry. Add a namespace by adding its en and pt-PT JSON files here;
// the parity, pseudo-locale and length checks pick it up automatically.
import authEn from "./locales/en/auth.json";
import commonEn from "./locales/en/common.json";
import legacyEn from "./locales/en/legacy.json";
import settingsEn from "./locales/en/settings.json";
import authPt from "./locales/pt-PT/auth.json";
import commonPt from "./locales/pt-PT/common.json";
import legacyPt from "./locales/pt-PT/legacy.json";
import settingsPt from "./locales/pt-PT/settings.json";

export const resources = {
  en: {
    common: commonEn,
    auth: authEn,
    settings: settingsEn,
    // The pre-V2 API-key dashboard; HOR-459 deletes it with LegacyDashboard.
    legacy: legacyEn,
  },
  "pt-PT": {
    common: commonPt,
    auth: authPt,
    settings: settingsPt,
    legacy: legacyPt,
  },
} as const;
