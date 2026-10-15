import { render, screen, waitFor } from "@testing-library/react";
import {
  afterAll,
  afterEach,
  beforeAll,
  describe,
  expect,
  it,
  vi,
} from "vitest";
import { SettingsHarness } from "../test/settingsHarness";
import PublicAuth from "../auth/PublicAuth";
import { PSEUDO_LOCALE, i18n, setPseudoLocale } from "./index";
import { PSEUDO_EXPANSION, pseudoLocalize } from "./pseudo";

// HOR-670 CI gate: rendered under the pseudo-locale, every piece of
// customer-facing copy must come out bracketed, accented and expanded. Any
// plain English left on screen was not taken from a catalog, and the expanded
// strings stand in for PT-PT's longer copy on the preview (?lng=en-XA).

const profile = {
  id: "identity-1",
  email: "ada@example.com",
  displayName: "Ada Lovelace",
  role: "admin" as const,
  locale: "en" as const,
  localePreference: "" as const,
  organizationLocale: "pt" as const,
  updatedAt: "2026-09-23T10:00:00Z",
};

// Data the person or the server supplied, never catalog copy.
const userData = new Set([profile.email, profile.displayName]);

function unlocalizedText(root: HTMLElement): string[] {
  const out: string[] = [];
  const check = (text: string | null | undefined) => {
    const value = text?.trim();
    if (!value || !/\p{L}/u.test(value) || userData.has(value)) return;
    if (!/^\[.*~+\]$/su.test(value) && !/\[[^\]]*~+\]/u.test(value))
      out.push(value);
  };
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  while (walker.nextNode()) check(walker.currentNode.textContent);
  for (const element of root.querySelectorAll("*")) {
    for (const name of ["aria-label", "placeholder", "title", "alt"])
      check(element.getAttribute(name));
  }
  return out;
}

beforeAll(() => setPseudoLocale(true));
afterAll(() => setPseudoLocale(false));
afterEach(() => vi.unstubAllGlobals());

describe("pseudo-locale", () => {
  it("brackets, accents and expands every catalog string", () => {
    expect(i18n.language).toBe(PSEUDO_LOCALE);
    const value = pseudoLocalize("Sign in {{name}}");
    expect(value).toMatch(/^\[Šíĝñ íñ \{\{name\}\}~+\]$/);
    expect(value.length).toBeGreaterThanOrEqual(
      "Sign in ".length * (1 + PSEUDO_EXPANSION),
    );
  });

  it("leaves no unlocalized copy on the sign-in journey", () => {
    const { container } = render(<PublicAuth onAuthenticated={vi.fn()} />);
    expect(screen.getAllByRole("heading").length).toBeGreaterThan(0);
    expect(unlocalizedText(container)).toEqual([]);
  });

  it("flags plain English when the pseudo-locale is off", () => {
    setPseudoLocale(false);
    try {
      const { container } = render(<PublicAuth onAuthenticated={vi.fn()} />);
      expect(unlocalizedText(container)).toContain("Sign in");
    } finally {
      setPseudoLocale(true);
    }
  });

  it("leaves no unlocalized copy in Settings → Profile", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() =>
        Promise.resolve(
          new Response("[]", {
            status: 200,
            headers: { "Content-Type": "application/json" },
          }),
        ),
      ),
    );
    const { container } = render(<SettingsHarness profile={profile} />);
    await waitFor(() => expect(container.querySelector("form")).not.toBeNull());
    expect(unlocalizedText(container)).toEqual([]);
  });
});
