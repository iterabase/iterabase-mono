import i18next, { type TFunction } from "i18next";
import { initReactI18next, useTranslation } from "react-i18next";
import { formatNumber } from "./format";
import { PSEUDO_LOCALE, pseudoCatalog } from "./pseudo";
import { resources } from "./resources";

// The customer app's localization foundation (DES-HOR-670-01): i18next with
// flat JSON catalogs per locale and namespace. English is the source catalog
// and PT-PT must be complete (src/i18n/catalog.test.ts). Product code works
// with the stored locale codes "en" | "pt"; catalogs and Intl use pt-PT.

export type Locale = "en" | "pt";
export type Namespace = keyof (typeof resources)["en"];
export const namespaces = Object.keys(resources.en) as Namespace[];
export { PSEUDO_LOCALE };

/** The catalog language for a stored locale code. */
export function languageTag(locale: Locale): string {
  return locale === "pt" ? "pt-PT" : "en";
}

/** Reads ?lng=en-XA so the preview can show the pseudo-locale. */
function pseudoRequested(): boolean {
  if (typeof window === "undefined") return false;
  try {
    return (
      new URLSearchParams(window.location.search).get("lng") === PSEUDO_LOCALE
    );
  } catch {
    return false;
  }
}

let pseudo = pseudoRequested();
let current = "en";

/** Turns the pseudo-locale on or off for every translation (tests, preview). */
export function setPseudoLocale(enabled: boolean): void {
  pseudo = enabled;
  void i18next.changeLanguage(enabled ? PSEUDO_LOCALE : current);
}

export function pseudoLocaleActive(): boolean {
  return pseudo;
}

void i18next.use(initReactI18next).init({
  resources: { ...resources, [PSEUDO_LOCALE]: pseudoCatalog(resources.en) },
  lng: pseudo ? PSEUDO_LOCALE : "en",
  fallbackLng: "en",
  supportedLngs: ["en", "pt-PT", PSEUDO_LOCALE],
  ns: namespaces,
  defaultNS: "common",
  fallbackNS: "common",
  interpolation: { escapeValue: false },
  returnNull: false,
  initAsync: false,
});

// "{{value, number}}" in a catalog formats with the locale's grouping
// (1.840 in PT, 1,840 in EN) while plural selection still uses the raw count.
i18next.services.formatter?.add("number", (value, lng) =>
  formatNumber(value, lng === "pt-PT" ? "pt" : "en"),
);

export const i18n = i18next;

/** Makes a locale the active UI language for hook-based components. */
export function setLocale(locale: Locale): void {
  current = languageTag(locale);
  if (typeof document !== "undefined") document.documentElement.lang = current;
  if (!pseudo) void i18next.changeLanguage(current);
}

/**
 * A translation function bound to a locale and namespace, for code that
 * receives the locale explicitly. Keys of the "common" namespace resolve from
 * any namespace.
 */
export function fixedT<N extends Namespace>(
  locale: Locale,
  ns: N,
): TFunction<N> {
  return i18next.getFixedT(pseudo ? PSEUDO_LOCALE : languageTag(locale), ns);
}

/** The browser's preferred supported locale, or English. */
export function browserLocale(): Locale {
  const languages =
    typeof navigator === "undefined"
      ? []
      : navigator.languages?.length
        ? navigator.languages
        : [navigator.language];
  for (const language of languages) {
    const primary = language?.toLowerCase().split("-")[0];
    if (primary === "pt" || primary === "en") return primary;
  }
  return "en";
}

export function isLocale(value: unknown): value is Locale {
  return value === "en" || value === "pt";
}

/** The active UI locale for components (the pseudo-locale reads as English). */
export function useLocale(): Locale {
  const { i18n: instance } = useTranslation();
  return instance.language === "pt-PT" ? "pt" : "en";
}
