// @vitest-environment node
import { parseAst } from "vite";
import { describe, expect, it } from "vitest";

// HOR-670 CI gate: customer-facing copy comes from the message catalogs, never
// from component source. Fails on JSX text, string children and user-facing
// attributes (aria-label, placeholder, title, alt, label) that contain letters.

// Every component's source, read at build time without Node APIs.
const sources = import.meta.glob<string>(
  ["../**/*.tsx", "!../**/*.test.tsx", "!./__fixtures__/**"],
  { query: "?raw", import: "default", eager: true },
);
const fixture = import.meta.glob<string>("./__fixtures__/*.tsx", {
  query: "?raw",
  import: "default",
  eager: true,
});
const copyAttributes = new Set([
  "aria-label",
  "aria-description",
  "placeholder",
  "title",
  "alt",
  "label",
]);

type Node = { type: string; [key: string]: unknown };

const hasWords = (value: string) => /\p{L}/u.test(value);

function isNode(value: unknown): value is Node {
  return !!value && typeof value === "object" && "type" in value;
}

/**
 * The literal strings an expression can render: string and template literals,
 * both branches of a conditional, and the fallbacks of `||`/`??`. Calls such as
 * t("key") are never followed, so translation keys are not treated as copy.
 */
function renderedStrings(expression: unknown): string[] {
  if (!isNode(expression)) return [];
  switch (expression.type) {
    case "Literal":
      return typeof expression.value === "string" ? [expression.value] : [];
    case "TemplateLiteral":
      return (expression.quasis as Array<{ value: { cooked: string } }>).map(
        (quasi) => quasi.value.cooked,
      );
    case "ConditionalExpression":
      return [
        ...renderedStrings(expression.consequent),
        ...renderedStrings(expression.alternate),
      ];
    case "LogicalExpression":
      return [
        ...(expression.operator === "&&"
          ? []
          : renderedStrings(expression.left)),
        ...renderedStrings(expression.right),
      ];
    case "ParenthesizedExpression":
    case "TSAsExpression":
      return renderedStrings(expression.expression);
    default:
      return [];
  }
}

function findings(code: string): string[] {
  const out: string[] = [];
  const line = (node: Node) =>
    code.slice(0, Number(node.start ?? 0)).split("\n").length;
  const visit = (node: unknown, parent?: Node) => {
    if (Array.isArray(node)) {
      node.forEach((child) => visit(child, parent));
      return;
    }
    if (!isNode(node)) return;
    if (node.type === "JSXText" && hasWords(String(node.value))) {
      out.push(`${line(node)}: text "${String(node.value).trim()}"`);
    }
    if (
      node.type === "JSXAttribute" &&
      isNode(node.name) &&
      copyAttributes.has(String(node.name.name)) &&
      isNode(node.value)
    ) {
      const values =
        node.value.type === "JSXExpressionContainer"
          ? renderedStrings(node.value.expression)
          : renderedStrings(node.value);
      for (const value of values.filter(hasWords))
        out.push(`${line(node)}: ${String(node.name.name)}="${value}"`);
    }
    if (
      node.type === "JSXExpressionContainer" &&
      parent &&
      (parent.type === "JSXElement" || parent.type === "JSXFragment")
    ) {
      for (const value of renderedStrings(node.expression).filter(hasWords))
        out.push(`${line(node)}: string child "${value}"`);
    }
    for (const [key, value] of Object.entries(node)) {
      if (key !== "parent") visit(value, node);
    }
  };
  visit(parseAst(code, { lang: "tsx" }));
  return out;
}

describe("hard-coded copy", () => {
  it("scans the app's components", () => {
    expect(Object.keys(sources)).toContain("../App.tsx");
  });

  for (const [file, code] of Object.entries(sources)) {
    it(`${file} takes its copy from the catalogs`, () => {
      expect(findings(code)).toEqual([]);
    });
  }

  it("catches copy written straight into a component", () => {
    expect(findings(fixture["./__fixtures__/Hardcoded.fixture.tsx"])).toEqual([
      '12: text "Save changes"',
      '13: aria-label="Close dialog"',
      '14: string child "Cancel"',
      '16: string child "Saving"',
      '16: string child "Save"',
      '17: placeholder="Work email"',
      '18: placeholder="Search "',
      '19: string child "Unknown"',
    ]);
  });
});
