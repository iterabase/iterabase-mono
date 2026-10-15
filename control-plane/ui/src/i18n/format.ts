import type { Locale } from "./index";

// Locale-aware formatting for every customer-facing date, time, number and
// amount (HOR-670). PT-PT follows the approved reference forms rather than raw
// CLDR, which groups with a narrow space, leaves four-digit numbers ungrouped
// and abbreviates "13 out." as "13/10":
//   PT: 1.840 €   ·   seg., 13 out.   ·   13 out. 2026, 15:05 (24-hour)
//   EN: €1,840 / $1,840 ·   Mon 13 Oct      ·   13 Oct 2026, 15:05

function toDate(value: Date | string | number): Date {
  return value instanceof Date ? value : new Date(value);
}

function intlTag(locale: Locale): string {
  return locale === "pt" ? "pt-PT" : "en-GB";
}

function ptMonth(date: Date, timeZone?: string): string {
  return new Intl.DateTimeFormat("pt-PT", { month: "short", timeZone }).format(
    date,
  );
}

function ptDay(date: Date, timeZone?: string): string {
  return new Intl.DateTimeFormat("pt-PT", { day: "numeric", timeZone }).format(
    date,
  );
}

function ptYear(date: Date, timeZone?: string): string {
  return new Intl.DateTimeFormat("pt-PT", { year: "numeric", timeZone }).format(
    date,
  );
}

/** Groups thousands with "." in PT (1.840) and keeps CLDR elsewhere. */
function formatParts(parts: Intl.NumberFormatPart[], locale: Locale): string {
  return parts
    .map((part) =>
      locale === "pt" && part.type === "group" ? "." : part.value,
    )
    .join("");
}

export function formatNumber(
  value: number | string,
  locale: Locale,
  options: Intl.NumberFormatOptions = {},
): string {
  const number = Number(value);
  if (!Number.isFinite(number)) return String(value);
  const format = new Intl.NumberFormat(intlTag(locale), {
    useGrouping: "always",
    ...options,
  } as Intl.NumberFormatOptions);
  return formatParts(format.formatToParts(number), locale);
}

/** Amounts drop ",00"/".00" when whole: 1.840 € / €1,840; 1.840,50 €. */
export function formatMoney(
  amount: number | string,
  currency: string,
  locale: Locale,
): string {
  return formatNumber(amount, locale, {
    style: "currency",
    currency: currency || "EUR",
    currencyDisplay: "narrowSymbol",
    maximumFractionDigits: 2,
    trailingZeroDisplay: "stripIfInteger",
  } as Intl.NumberFormatOptions);
}

/** 24-hour time: 15:05. */
export function formatTime(
  value: Date | string | number,
  locale: Locale,
  timeZone?: string,
): string {
  return new Intl.DateTimeFormat(intlTag(locale), {
    hour: "2-digit",
    minute: "2-digit",
    hourCycle: "h23",
    timeZone,
  }).format(toDate(value));
}

/** Day and month: 13 out. / 13 Oct. */
export function formatShortDate(
  value: Date | string | number,
  locale: Locale,
  timeZone?: string,
): string {
  const date = toDate(value);
  if (locale === "pt")
    return `${ptDay(date, timeZone)} ${ptMonth(date, timeZone)}`;
  return new Intl.DateTimeFormat("en-GB", {
    day: "numeric",
    month: "short",
    timeZone,
  }).format(date);
}

/** Weekday, day and month: seg., 13 out. / Mon 13 Oct. */
export function formatDay(
  value: Date | string | number,
  locale: Locale,
  timeZone?: string,
): string {
  const date = toDate(value);
  if (locale === "pt") {
    // pt-PT CLDR abbreviates weekdays as "segunda"; the approved form is the
    // short "seg.".
    const weekday = new Intl.DateTimeFormat("pt", {
      weekday: "short",
      timeZone,
    }).format(date);
    return `${weekday}, ${formatShortDate(date, locale, timeZone)}`;
  }
  return new Intl.DateTimeFormat("en-GB", {
    weekday: "short",
    day: "numeric",
    month: "short",
    timeZone,
  }).format(date);
}

/** Full date and time: 13 out. 2026, 15:05 / 13 Oct 2026, 15:05. */
export function formatDateTime(
  value: Date | string | number | undefined,
  locale: Locale,
  timeZone?: string,
): string {
  if (value === undefined || value === "") return "";
  const date = toDate(value);
  if (Number.isNaN(date.getTime())) return String(value);
  const time = formatTime(date, locale, timeZone);
  if (locale === "pt")
    return `${formatShortDate(date, locale, timeZone)} ${ptYear(date, timeZone)}, ${time}`;
  const day = new Intl.DateTimeFormat("en-GB", {
    day: "numeric",
    month: "short",
    year: "numeric",
    timeZone,
  }).format(date);
  return `${day}, ${time}`;
}

/** Weekday, day and month in full: Monday 13 October / segunda-feira, 13 de outubro. */
export function formatLongDay(
  value: Date | string | number,
  locale: Locale,
  timeZone?: string,
): string {
  const date = toDate(value);
  if (locale === "pt")
    return new Intl.DateTimeFormat("pt-PT", {
      weekday: "long",
      day: "numeric",
      month: "long",
      timeZone,
    }).format(date);
  const weekday = new Intl.DateTimeFormat("en-GB", {
    weekday: "long",
    timeZone,
  }).format(date);
  const day = new Intl.DateTimeFormat("en-GB", {
    day: "numeric",
    month: "long",
    timeZone,
  }).format(date);
  return `${weekday} ${day}`;
}
