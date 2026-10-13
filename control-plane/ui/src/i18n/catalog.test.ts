import { describe, expect, it } from "vitest";
import { lengthBudgets } from "./limits";
import { resources } from "./resources";

// HOR-670 CI gate: English is the source catalog and PT-PT must be complete.
// A missing, extra or empty PT key, a placeholder mismatch, a missing plural
// form or a glossary violation fails the build.

type Catalog = { [key: string]: string | Catalog };

function flatten(catalog: Catalog, prefix = ""): Map<string, string> {
  const out = new Map<string, string>();
  for (const [key, value] of Object.entries(catalog)) {
    const path = prefix ? `${prefix}.${key}` : key;
    if (typeof value === "string") out.set(path, value);
    else for (const entry of flatten(value, path)) out.set(...entry);
  }
  return out;
}

const pluralSuffix = /_(zero|one|two|few|many|other)$/;

/** Groups plural forms under their base key. */
function baseKeys(keys: Iterable<string>): Map<string, Set<string>> {
  const out = new Map<string, Set<string>>();
  for (const key of keys) {
    const match = key.match(pluralSuffix);
    const base = match ? key.slice(0, -match[0].length) : key;
    const forms = out.get(base) ?? new Set<string>();
    if (match) forms.add(match[1]);
    out.set(base, forms);
  }
  return out;
}

function placeholders(value: string): string[] {
  return [...value.matchAll(/\{\{\s*([\w.]+)[^}]*\}\}/g)]
    .map((match) => match[1])
    .sort();
}

const languages = { en: "en", "pt-PT": "pt-PT" } as const;

// Forbidden words per locale: "AI coworker", never "agent"; formal PT-PT
// without "você"; no Brazilian terms the glossary rules out.
const forbidden = {
  en: ["agents?"],
  "pt-PT": [
    "vocês?",
    "agentes?",
    "time",
    "arquivos?",
    "senhas?",
    "usuári[oa]s?",
    "telas?",
    "cadastr\\p{L}*",
  ],
} as const;

/**
 * Words in `value` the glossary forbids. Boundaries are Unicode-aware: an
 * ASCII \\b does not see the end of "você".
 */
function glossaryViolations(
  value: string,
  lang: keyof typeof forbidden,
): string[] {
  const pattern = new RegExp(
    `(?<![\\p{L}\\p{N}])(?:${forbidden[lang].join("|")})(?![\\p{L}\\p{N}])`,
    "giu",
  );
  return [...value.matchAll(pattern)].map((match) => match[0]);
}
const namespaces = Object.keys(resources.en) as Array<
  keyof typeof resources.en
>;

describe("message catalogs", () => {
  it("registers the same namespaces for every locale", () => {
    expect(Object.keys(resources["pt-PT"]).sort()).toEqual(
      [...namespaces].sort(),
    );
  });

  for (const ns of namespaces) {
    const en = flatten(resources.en[ns] as Catalog);
    const pt = flatten(resources["pt-PT"][ns] as Catalog);
    const enBase = baseKeys(en.keys());
    const ptBase = baseKeys(pt.keys());

    it(`${ns}: PT-PT has exactly the English keys`, () => {
      expect([...ptBase.keys()].sort()).toEqual([...enBase.keys()].sort());
    });

    it(`${ns}: every plural key has each locale's plural forms`, () => {
      for (const [lang, base] of [
        [languages.en, enBase],
        [languages["pt-PT"], ptBase],
      ] as const) {
        const required = new Intl.PluralRules(lang).resolvedOptions()
          .pluralCategories;
        for (const [key, forms] of base) {
          if (forms.size === 0) continue;
          expect([...forms].sort(), `${lang} ${ns}:${key}`).toEqual(
            [...required].sort(),
          );
        }
      }
    });

    it(`${ns}: no empty strings and matching placeholders`, () => {
      for (const [key, value] of [...en, ...pt]) {
        expect(value.trim(), `${ns}:${key}`).not.toBe("");
      }
      for (const [base] of enBase) {
        const variants = (catalog: Map<string, string>) =>
          [...catalog]
            .filter(
              ([key]) => key === base || key.replace(pluralSuffix, "") === base,
            )
            .map(([, value]) =>
              placeholders(value).filter((p) => p !== "count"),
            );
        const expected = variants(en)[0];
        for (const found of [...variants(en), ...variants(pt)]) {
          expect(found, `${ns}:${base}`).toEqual(expected);
        }
      }
    });

    it(`${ns}: copy follows the approved glossary`, () => {
      for (const [key, value] of en)
        expect(glossaryViolations(value, "en"), `en ${ns}:${key}`).toEqual([]);
      for (const [key, value] of pt)
        expect(
          glossaryViolations(value, "pt-PT"),
          `pt-PT ${ns}:${key}`,
        ).toEqual([]);
    });
  }
});

describe("glossary check", () => {
  it("rejects forbidden words, including ones ending in accented letters", () => {
    expect(glossaryViolations("Confirme que é você", "pt-PT")).toEqual([
      "você",
    ]);
    expect(glossaryViolations("Vocês podem entrar", "pt-PT")).toEqual([
      "Vocês",
    ]);
    expect(glossaryViolations("O agente respondeu", "pt-PT")).toEqual([
      "agente",
    ]);
    expect(glossaryViolations("Digite a senha", "pt-PT")).toEqual(["senha"]);
    expect(glossaryViolations("Ask the agent", "en")).toEqual(["agent"]);
    expect(glossaryViolations("Confirme a sua identidade", "pt-PT")).toEqual(
      [],
    );
    expect(glossaryViolations("Agentes? Não: agentesque", "pt-PT")).toEqual([
      "Agentes",
    ]);
  });
});

describe("length budgets", () => {
  // Compact controls (tabs, buttons, chips) must hold their copy in every
  // locale; PT-PT runs 15–30% longer than English.
  it("keeps compact copy within its budget in every locale", () => {
    for (const [id, max] of Object.entries(lengthBudgets)) {
      const [ns, key] = id.split(":") as [keyof typeof resources.en, string];
      for (const lang of Object.values(languages)) {
        const value = flatten(resources[lang][ns] as Catalog).get(key);
        expect(value, `${lang} ${id} exists`).toBeDefined();
        expect(value!.length, `${lang} ${id} "${value}"`).toBeLessThanOrEqual(
          max,
        );
      }
    }
  });
});
