# V2 customer app localization (EN and PT-PT)

- **Status:** Approved design, implemented by HOR-670.
- **Approval date:** 2026-10-13
- **Ticket:** [HOR-670](https://linear.app/horizonshift/issue/HOR-670/v2-en-and-pt-pt-localization-foundation-for-the-customer-app)
- **Approved decisions:** `DES-HOR-670-01` (library and catalogs), `DES-HOR-670-02` (preference storage and resolution), `DES-HOR-670-03` (Teams card language). The canonical records are in the HOR-670 Linear issue.
- **Related authority:** [`v2-authentication-authority.md`](v2-authentication-authority.md) (profile, organisation settings, audit).

Every customer-facing surface ships in English and European Portuguese (PT-PT). A UI ticket is not done with English only. This record states the contract later tickets inherit; it does not restate the PT-PT glossary, which lives in the HOR-670 issue.

## 1. Message catalogs (DES-HOR-670-01)

- The control-plane UI uses `i18next` and `react-i18next`. Catalogs are JSON, one file per locale and namespace, under `control-plane/ui/src/i18n/locales/{en,pt-PT}/<namespace>.json`, registered in `src/i18n/resources.ts`.
- English is the source catalog. Keys are type-checked against it (`src/i18n/i18next.d.ts`), so a missing English key fails `tsc`.
- Namespaces follow surfaces: `common` (shared, and the fallback namespace for every other one), `auth` (public sign-in journey), `settings` (the account panels hosted by Settings: Profile, Sessions, Admin), `app` (the app frame, navigation, trust line and shared states), `work` (Today, Your turn, Work and the work item) and `team` (Team and AI coworker detail). HOR-459 removed the pre-V2 `legacy` namespace with the old dashboard. New screens add their own namespace.
- Components use `useTranslation(ns)` or, where the locale is passed explicitly, `fixedT(locale, ns)`. Product code keeps the stored codes `en | pt`; catalogs and `Intl` use `en` / `pt-PT`.
- Plurals use i18next suffixes. PT-PT needs `_one`, `_many` and `_other` (CLDR); English needs `_one` and `_other`.
- Formatting goes through `src/i18n/format.ts`, never raw `Intl` in components. PT-PT follows the approved reference forms rather than raw CLDR: `1.840 €`, `seg., 13 out.`, `13 out. 2026, 15:05` (24-hour). English: `€1,840`, `$1,840`, `Mon 13 Oct`.
- Server-provided customer copy (`LocalizedText` with `en`/`pt`) is picked per locale; it is data, not catalog copy.

## 2. CI checks

`make ui-test` (`npm test`) fails when any of these breaks:

| Check | File | Fails on |
| --- | --- | --- |
| Catalog parity | `src/i18n/catalog.test.ts` | a PT-PT key missing or extra, an empty string, mismatched `{{placeholders}}`, a missing plural form |
| Glossary | `src/i18n/catalog.test.ts` | "agent" in English, "agente"/"você" or the listed Brazilian terms in PT-PT |
| Length budgets | `src/i18n/catalog.test.ts`, `src/i18n/limits.ts` | copy in compact controls (tabs, buttons, status chips) longer than its budget in any locale |
| Hard-coded copy | `src/i18n/hardcoded.test.ts` | JSX text, string children or `aria-label`/`placeholder`/`title`/`alt`/`label` literals with letters in any component |
| Pseudo-locale | `src/i18n/pseudo.test.tsx` | any copy that renders without the pseudo-locale's brackets and accents |

The pseudo-locale `en-XA` makes every string about 40% longer, bracketed and accented. Add `?lng=en-XA` to a preview URL to review layouts against long copy; PT-PT runs 15–30% longer than English.

## 3. Language preference and resolution (DES-HOR-670-02)

- `identity.local_users.locale` is `en | pt | NULL`. `NULL` follows the organisation default. A choice made at access request, setup or in Profile is explicit; an account created without one (for example by an Admin) follows the organisation.
- `identity.organization_settings` is a single row (one deployment is one customer organisation). `default_locale` is `en | pt | NULL` and stays `NULL` until an Admin chooses one. Only Admins read or change it (`GET`/`PATCH /v1/organization/settings`, CSRF, optimistic `expectedUpdatedAt`), and every change appends an `organization_settings_updated` security event with the Admin as actor.
- Web requests resolve the effective language as: person's choice → organisation default → browser `Accept-Language` → English (`identity.ResolveWebLocale`). `/v1/auth/session` returns it before and after sign-in; the profile response carries `locale` (effective), `localePreference` (own choice, `""` follows the organisation) and `organizationLocale`.
- Server-side work with no browser resolves a recipient with `identity.Store.ResolveLocale(ctx, identityID)`: person's choice → organisation default → English. Authentication emails to existing accounts already use it.

### Obligations for later tickets

| Surface | Rule | Owner |
| --- | --- | --- |
| Sofia | Replies in the person's language from `ResolveLocale`. | HOR-452 |
| Push notifications | Text in the recipient's language from `ResolveLocale`; quoted content is never translated. | HOR-664 |
| Teams 1:1 bot chat | The recipient's language from `ResolveLocale`. | HOR-248, HOR-659 |
| Teams group chat or channel card | The organisation default (not the requester's or approver's language); card updates keep the card's language (DES-HOR-670-03). | HOR-248 |
| Outbound customer email | Keeps the thread or template language. Never calls `ResolveLocale` and never follows the Operator's UI language. | HOR-599 |

In every notification and card, only Iterabase's own labels and actions are localised. Customer emails, supplier replies and document excerpts quoted in them stay exactly as written.
