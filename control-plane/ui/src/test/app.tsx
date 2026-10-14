import { createMemoryHistory } from "@tanstack/react-router";
import { render } from "@testing-library/react";
import axe from "axe-core";
import { expect, vi } from "vitest";
import App from "../App";
import type { AuthProfile } from "../auth/authApi";
import { createMockApi, operator, type MockApi } from "./mockApi";

// Renders the whole app at a path over the in-memory customer API.
export function startApp(
  path = "/",
  profile: AuthProfile = operator,
  overrides: Parameters<typeof createMockApi>[1] = {},
): { api: MockApi; history: ReturnType<typeof createMemoryHistory> } {
  const api = createMockApi(profile, overrides);
  vi.stubGlobal("fetch", vi.fn(api.handle));
  const history = createMemoryHistory({ initialEntries: [path] });
  render(<App history={history} />);
  return { api, history };
}

/** Structural axe rules; contrast follows the Design System token baseline. */
export async function expectAccessible(root: HTMLElement = document.body) {
  const result = await axe.run(root, {
    rules: { "color-contrast": { enabled: false } },
  });
  expect(result.violations.map((violation) => violation.id)).toEqual([]);
}
