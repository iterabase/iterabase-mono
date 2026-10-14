// Length budgets for copy in compact controls ("namespace:key" → max
// characters), checked for every locale in catalog.test.ts. A budget states
// what the layout was designed to hold; a translation that outgrows it fails CI
// instead of truncating a key action. New screens add their tabs, buttons and
// chips here.

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

  "app:nav.today": TAB,
  "app:nav.work": TAB,
  "app:nav.team": TAB,
  "app:nav.more": TAB,
  "app:signOut": BUTTON,
  "app:retry": BUTTON,

  "work:state.blocked": STATUS,
  "work:state.done": STATUS,
  "work:state.failed": STATUS,
  "work:state.in_progress": STATUS,
  "work:state.needsAnswer": STATUS,
  "work:state.needsOk": STATUS,
  "work:state.paused": STATUS,
  "work:state.todo": STATUS,
  "work:state.waiting": STATUS,
  "work:group.working": TAB,
  "work:group.waiting": TAB,
  "work:group.needs": TAB,
  "work:group.done": TAB,
  "work:period.today": TAB,
  "work:period.week": TAB,
  "work:period.month": TAB,
  "work:today.startFirst": BUTTON,
  "work:item.stopWaiting": BUTTON,
  "work:item.restartWithGuidance": BUTTON,
  "work:item.somethingWrong": BUTTON,
  "work:turn.openItem": BUTTON,
  "work:allClear.backToToday": BUTTON,
  "work:allClear.seeAllWork": BUTTON,
  "work:feedback.save": BUTTON,
  "work:failed.outcomeUnknown": STATUS,

  "team:tabs.how": TAB,
  "team:tabs.can": TAB,
  "team:tabs.activity": TAB,
  "team:tabs.failed": TAB,
  "team:filter.all": TAB,
  "team:filter.needs": TAB,
  "team:filter.done": TAB,
  "team:filter.failed": TAB,
  "team:youConfirm": STATUS,
};
