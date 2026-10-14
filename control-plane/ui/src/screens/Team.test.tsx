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

// Team and AI coworker detail (HOR-459).

describe("Team", () => {
  it("shows each AI coworker from its workflow definition", async () => {
    start("/team");
    const card = await screen.findByRole("article", { name: "Quotes" });
    expect(
      within(card).getByText("Sales desk · AI coworker"),
    ).toBeInTheDocument();
    expect(
      within(card).getByText("Reads customer emails and PDFs"),
    ).toBeInTheDocument();
    // "Always asks you" comes from the human gates, never declared copy.
    expect(
      within(card).getByText("Before any email to a customer"),
    ).toBeInTheDocument();
    expect(screen.getByRole("article", { name: "Rates" })).toBeInTheDocument();
  });

  it("opens a coworker's How it works, What it can do and Activity", async () => {
    const user = userEvent.setup();
    start(`/team/${encodeURIComponent(fx.quotes.key)}`);
    expect(
      await screen.findByRole("heading", { level: 1, name: "Quotes" }),
    ).toBeInTheDocument();
    expect(screen.getByText("Collects supplier replies")).toBeInTheDocument();
    await user.click(screen.getByRole("link", { name: /What it can do/ }));
    expect(
      await screen.findByText(
        "Anything not on these lists also waits for your OK.",
      ),
    ).toBeInTheDocument();
    await user.click(screen.getByRole("link", { name: "Activity" }));
    await user.click(await screen.findByRole("button", { name: /Needs you/ }));
    const table = await screen.findByRole("table");
    // A header row plus the two items waiting on a person.
    await waitFor(() =>
      expect(within(table).getAllByRole("row")).toHaveLength(3),
    );
  });
});
