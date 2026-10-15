import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setLocale, setPseudoLocale } from "../i18n";
import * as fx from "../test/fixtures";
import { admin, operator, type MockApi } from "../test/mockApi";
import { expectAccessible, startApp } from "../test/app";

let api: MockApi;
function start(...args: Parameters<typeof startApp>) {
  const started = startApp(...args);
  api = started.api;
  return started;
}
const a11y = expectAccessible;

beforeEach(() => setLocale("en"));
afterEach(() => {
  vi.unstubAllGlobals();
  setLocale("en");
});

// The app frame and authority: denial, Admin-only screens, session loss.

describe("Authority", () => {
  it("denies an Operator's direct URL to Admin settings", async () => {
    start("/settings/people", operator);
    expect(
      await screen.findByRole("heading", {
        name: "This page isn't available to you",
      }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("link", { name: "People" }),
    ).not.toBeInTheDocument();
  });

  it("shows an Admin the People settings", async () => {
    start("/settings/people", admin);
    expect(await screen.findByRole("link", { name: "People" })).toHaveAttribute(
      "aria-current",
      "page",
    );
    expect(
      screen.queryByText("This page isn't available to you"),
    ).not.toBeInTheDocument();
  });

  it("returns to sign-in when the session is lost", async () => {
    start("/", operator, { eventsStatus: 401 });
    expect(
      await screen.findByRole("button", { name: "Sign in" }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("navigation", { name: "Main" }),
    ).not.toBeInTheDocument();
  });
});

describe("Installation name (DES-HOR-459-08)", () => {
  it("shows it on the sign-in page and in the top bar", async () => {
    start("/", operator, { anonymous: true });
    expect(await screen.findByText("Transitários Exemplo")).toBeInTheDocument();
  });

  it("shows it after the logo once signed in", async () => {
    start("/");
    const banner = await screen.findByRole("banner");
    expect(
      await within(banner).findByText("Transitários Exemplo"),
    ).toBeInTheDocument();
  });

  it("shows no name when the installation has none", async () => {
    start("/", operator, { organizationName: "" });
    const banner = await screen.findByRole("banner");
    await within(banner).findByRole("navigation", { name: "Main" });
    expect(
      within(banner).queryByText("Transitários Exemplo"),
    ).not.toBeInTheDocument();
  });
});
