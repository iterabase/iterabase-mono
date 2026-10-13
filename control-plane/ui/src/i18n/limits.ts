// Length budgets for copy in compact controls ("namespace:key" → max
// characters), checked for every locale in catalog.test.ts. A budget states
// what the layout was designed to hold; a translation that outgrows it fails CI
// instead of truncating a key action. New screens add their tabs, buttons and
// chips here (HOR-459: Hoje · Trabalho · Equipa · Relatórios · Definições).

const TAB = 20;
const BUTTON = 32;
const STATUS = 26;

export const lengthBudgets: Record<string, number> = {
  "common:languageShort.en": 2,
  "common:languageShort.pt": 2,
  "common:retry": BUTTON,

  "auth:signIn": BUTTON,
  "auth:requestSubmit": BUTTON,
  "auth:forgotSubmit": BUTTON,
  "auth:finishSetup": BUTTON,
  "auth:resetSubmit": BUTTON,
  "auth:requestAccessLink": BUTTON,
  "auth:forgotPassword": BUTTON,

  "settings:navProfile": TAB,
  "settings:navSessions": TAB,
  "settings:navRequests": TAB,
  "settings:signOut": BUTTON,
  "settings:profileSave": BUTTON,
  "settings:revokeConfirm": BUTTON,
  "settings:signOutSession": BUTTON,
  "settings:requestsReview": BUTTON,
  "settings:requestsDecline": BUTTON,
  "settings:approveSubmit": BUTTON,
  "settings:organizationLanguageSave": BUTTON,

  "legacy:todo": STATUS,
  "legacy:in_progress": STATUS,
  "legacy:waiting": STATUS,
  "legacy:paused": STATUS,
  "legacy:blocked": STATUS,
  "legacy:done": STATUS,
  "legacy:failedState": STATUS,
  "legacy:open": BUTTON,
  "legacy:download": BUTTON,
  "legacy:stopWaiting": BUTTON,
};
