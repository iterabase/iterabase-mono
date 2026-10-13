// Pseudo-locale (HOR-670 CI check): every catalog string becomes accented,
// about 40% longer and bracketed, so tests and the preview (?lng=en-XA) show
// both hard-coded copy (unbracketed) and layouts that break when a string runs
// longer than English, as PT-PT does by 15–30%.

export const PSEUDO_LOCALE = "en-XA";

const plainLetters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ";
const accentedLetters = [
  ..."áƀçďéƒĝĥíĵķĺɱñóþǫŕšţúṽŵẋýžÁƁÇĎÉƑĜĤÍĴĶĹṀÑÓÞǪŔŠŢÚṼŴẊÝŽ",
];
const accents: Record<string, string> = Object.fromEntries(
  [...plainLetters].map((char, index) => [char, accentedLetters[index]]),
);

/** Expansion applied to every pseudo-localized string. */
export const PSEUDO_EXPANSION = 0.4;

export function pseudoLocalize(value: string): string {
  // Interpolations ({{name}}) and nesting ($t(key)) must survive untouched.
  const pieces = value.split(/(\{\{[^}]+\}\}|\$t\([^)]*\))/);
  const body = pieces
    .map((piece, index) =>
      index % 2 === 1
        ? piece
        : piece.replace(/[A-Za-z]/g, (char) => accents[char] ?? char),
    )
    .join("");
  const visible = value.replace(/\{\{[^}]+\}\}|\$t\([^)]*\)/g, "").length;
  const padding = "~".repeat(Math.ceil(visible * PSEUDO_EXPANSION));
  return `[${body}${padding}]`;
}

type Catalog = { [key: string]: string | Catalog };

export function pseudoCatalog<T extends Catalog>(catalog: T): T {
  return Object.fromEntries(
    Object.entries(catalog).map(([key, value]) => [
      key,
      typeof value === "string" ? pseudoLocalize(value) : pseudoCatalog(value),
    ]),
  ) as T;
}
