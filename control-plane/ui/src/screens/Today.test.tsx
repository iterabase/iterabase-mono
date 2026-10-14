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

// Today, Your turn and All clear (HOR-459).

describe("Today", () => {
  it.each([
    ["an Operator", operator],
    ["an Admin", admin],
  ])("is where %s lands after signing in", async (_, profile) => {
    start("/", profile);
    expect(
      await screen.findByRole("heading", {
        level: 1,
        name: /, (Rita|Nadia)\.$/,
      }),
    ).toBeInTheDocument();
    const yourTurn = await screen.findByRole("region", { name: "Your turn" });
    expect(within(yourTurn).getByText("2")).toBeInTheDocument();
    expect(
      within(yourTurn).getByRole("link", { name: /Start with the first one/ }),
    ).toHaveAttribute("href", "/your-turn");
    // The team right now, from the workflow definitions.
    const team = screen.getByRole("region", { name: "Your team right now" });
    expect(within(team).getByText("Quotes")).toBeInTheDocument();
    expect(within(team).getByText("Rates")).toBeInTheDocument();
    // Waiting for you lists only human actions, oldest first.
    const waitingForYou = screen.getByRole("region", {
      name: "Waiting for you",
    });
    expect(
      within(waitingForYou)
        .getAllByRole("link")
        .map((row) => row.textContent),
    ).toEqual([
      expect.stringContaining(fx.question.title),
      expect.stringContaining(fx.approval.title),
    ]);
    expect(
      await screen.findByText("You got 1 h 50 back so far today (estimate)"),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("link", { name: "Failed · 1" }),
    ).toBeInTheDocument();
    expect(
      screen.getAllByText(/Nothing leaves without your OK/).length,
    ).toBeGreaterThan(0);
  });

  it("shows Sofia and Reports nowhere until they are built (DES-HOR-459-03)", async () => {
    start("/", admin);
    const nav = await screen.findByRole("navigation", { name: "Main" });
    expect(
      within(nav)
        .getAllByRole("link")
        .map((link) => link.textContent),
    ).toEqual(["Today", expect.stringMatching(/^Work/), "Team"]);
  });

  it("has no accessibility violations", async () => {
    start("/");
    await screen.findByRole("region", { name: "Waiting for you" });
    await a11y();
  });

  it("renders in European Portuguese", async () => {
    start("/", { ...operator, locale: "pt", localePreference: "pt" });
    expect(
      await screen.findByRole("heading", { name: "A sua vez" }),
    ).toBeInTheDocument();
    const nav = screen.getByRole("navigation", { name: "Principal" });
    expect(within(nav).getByRole("link", { name: "Hoje" })).toBeInTheDocument();
    expect(
      await screen.findByText("Recuperou 1 h 50 até agora, hoje (estimativa)"),
    ).toBeInTheDocument();
  });
});

describe("Your turn", () => {
  it("clears decisions one at a time and ends in All clear", async () => {
    const user = userEvent.setup();
    start("/your-turn");
    // Oldest first: the question, then the approval.
    expect(await screen.findByText("Decision 1 of 2")).toBeInTheDocument();
    expect(
      await screen.findByRole("heading", {
        level: 1,
        name: "Which weight: 1.2 t or 7.2 t?",
      }),
    ).toBeInTheDocument();
    const send = screen.getByRole("button", { name: /Send answer/ });
    expect(send).toBeDisabled();
    await user.selectOptions(screen.getByLabelText("Cargo weight"), "1");
    await user.click(send);

    expect(await screen.findByText("Decision 2 of 2")).toBeInTheDocument();
    expect(
      await screen.findByRole("heading", {
        level: 1,
        name: "Send this quote to Rui Costa?",
      }),
    ).toBeInTheDocument();
    // The exact content and its provenance are in front of the person.
    expect(
      screen.getByRole("region", { name: "Exact content" }),
    ).toHaveTextContent("RE: Pedido de cotação LCL Leixões–Santos");
    expect(screen.getByText(/^Prepared by/).closest("p")).toHaveTextContent(
      /Prepared by Quotes · \d\d:\d\d · Picks the best rate/,
    );
    // A approves, mirroring the visible primary button.
    expect(
      screen.getByRole("button", { name: /Approve & send/ }),
    ).toBeEnabled();
    await user.keyboard("a");

    expect(
      await screen.findByRole("heading", {
        name: "That's everything for now.",
      }),
    ).toBeInTheDocument();
    expect(screen.getAllByText("Answered by Rita Mendes")).toHaveLength(2);

    const responses = api.calls.filter((call) =>
      call.path.startsWith("/v1/work-blockers/"),
    );
    expect(responses.map((call) => [call.path, call.body])).toEqual([
      [
        "/v1/work-blockers/b-1045/responses",
        expect.objectContaining({
          outcome: "answered",
          response: { weight: "7.2t" },
        }),
      ],
      [
        "/v1/work-blockers/b-1039/responses",
        expect.objectContaining({ outcome: "approved", response: {} }),
      ],
    ]);
    for (const call of responses)
      expect(call.headers["x-csrf-token"]).toBe("csrf-1");
  });
});

describe("Pseudo-locale", () => {
  it("leaves no unlocalized copy on Today", async () => {
    setPseudoLocale(true);
    try {
      start("/");
      await screen.findByText(fx.approval.title);
      await screen.findAllByText("Quotes");
      const data = [
        operator.displayName,
        "RM",
        ...fx.items.flatMap((item) => [item.title, item.source.title]),
        ...fx.workflows.map((workflow) => workflow.coworker.name),
      ];
      // Server-provided copy (workflow and blocker presentation) is data too.
      const serverCopy = JSON.stringify([fx.workflows, fx.blockers, fx.items]);
      const offenders: string[] = [];
      const walker = document.createTreeWalker(
        document.body,
        NodeFilter.SHOW_TEXT,
      );
      while (walker.nextNode()) {
        const text = walker.currentNode.textContent?.trim() ?? "";
        if (
          !/\p{L}/u.test(text) ||
          data.includes(text) ||
          serverCopy.includes(text)
        )
          continue;
        // Dates and times come from Intl, not the catalogs.
        if (/^\p{L}+ \d{1,2} \p{L}+ · \d\d:\d\d$/u.test(text)) continue;
        if (!/\[[^\]]*~+\]/u.test(text)) offenders.push(text);
      }
      expect(offenders).toEqual([]);
    } finally {
      setPseudoLocale(false);
    }
  });
});
