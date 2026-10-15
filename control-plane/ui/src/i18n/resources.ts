// Catalog registry. Add a namespace by adding its en and pt-PT JSON files here;
// the parity, pseudo-locale and length checks pick it up automatically.
import appEn from "./locales/en/app.json";
import authEn from "./locales/en/auth.json";
import commonEn from "./locales/en/common.json";
import settingsEn from "./locales/en/settings.json";
import teamEn from "./locales/en/team.json";
import todayEn from "./locales/en/today.json";
import workEn from "./locales/en/work.json";
import appPt from "./locales/pt-PT/app.json";
import authPt from "./locales/pt-PT/auth.json";
import commonPt from "./locales/pt-PT/common.json";
import settingsPt from "./locales/pt-PT/settings.json";
import teamPt from "./locales/pt-PT/team.json";
import todayPt from "./locales/pt-PT/today.json";
import workPt from "./locales/pt-PT/work.json";

export const resources = {
  en: {
    common: commonEn,
    auth: authEn,
    settings: settingsEn,
    app: appEn,
    today: todayEn,
    work: workEn,
    team: teamEn,
  },
  "pt-PT": {
    common: commonPt,
    auth: authPt,
    settings: settingsPt,
    app: appPt,
    today: todayPt,
    work: workPt,
    team: teamPt,
  },
} as const;
