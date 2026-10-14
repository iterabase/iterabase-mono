import { createMemoryHistory } from "@tanstack/react-router";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import axe from "axe-core";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import App from "./App";
import { setLocale, setPseudoLocale } from "./i18n";
import * as fx from "./test/fixtures";
import { admin, createMockApi, operator, type MockApi } from "./test/mockApi";

// The customer app foundation (HOR-459): both roles land on Today, clear
// decisions in Your turn, work items open in the side sheet, items move
// groups live, and admin-only screens are denied to Operators.

let api: MockApi;

function start(path = "/", profile = operator, overrides = {}) {
  api = createMockApi(profile, overrides);
  vi.stubGlobal("fetch", vi.fn(api.handle));
  const history = createMemoryHistory({ initialEntries: [path] });
  const utils = render(<App history={history} />);
  return { ...utils, history };
}

async function a11y(root: HTMLElement = document.body) {
  const result = await axe.run(root, {
    rules: { "color-contrast": { enabled: false } },
  });
  expect(result.violations.map((violation) => violation.id)).toEqual([]);
}

beforeEach(() => setLocale("en"));
afterEach(() => {
  vi.unstubAllGlobals();
  setLocale("en");
});

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

describe("Work", () => {
  it("groups work and keeps waiting and paused work out of Needs you", async () => {
    start("/work");
    const board = await screen.findByRole("region", { name: "Work" });
    const column = (name: string) =>
      within(board).getByRole("region", { name });
    expect(within(column("Needs you")).getAllByRole("link")).toHaveLength(2);
    const waitingColumn = column("Waiting");
    expect(
      within(waitingColumn).getByText(fx.waiting.title),
    ).toBeInTheDocument();
    expect(
      within(waitingColumn).getByText(fx.paused.title),
    ).toBeInTheDocument();
    expect(
      within(waitingColumn).getByText(
        "Paused: the AI provider isn't responding. It resumes by itself.",
      ),
    ).toBeInTheDocument();
    expect(
      within(column("Working")).getByText(fx.working.title),
    ).toBeInTheDocument();
  });

  it("shows only failed attempts in the Failed strip", async () => {
    start("/work");
    const strip = await screen.findByRole("region", { name: "Failed · 1" });
    expect(
      within(strip).getByText("A step couldn't finish"),
    ).toBeInTheDocument();
    expect(
      within(strip).getByRole("link", { name: fx.failed.title }),
    ).toBeInTheDocument();
    expect(within(strip).queryByText(fx.paused.title)).not.toBeInTheDocument();
    expect(
      within(strip).getByRole("link", { name: "Review restart" }),
    ).toHaveAttribute("href", `/work/${fx.failed.id}`);
  });

  it("opens an item in the side sheet and closes back to the board", async () => {
    const user = userEvent.setup();
    const { history } = start("/work?period=week");
    await user.click(
      await screen.findByRole("link", { name: new RegExp(fx.done.title) }),
    );
    const sheet = await screen.findByRole("dialog", { name: fx.done.title });
    expect(
      await within(sheet).findByText(/^Answered by Rita Mendes · \d\d:\d\d$/),
    ).toBeInTheDocument();
    expect(
      within(sheet).getByRole("heading", { name: "Timeline" }),
    ).toBeInTheDocument();
    expect(
      within(sheet).getByRole("button", { name: "Something was wrong" }),
    ).toBeInTheDocument();
    await a11y(sheet);
    await user.keyboard("{Escape}");
    await waitFor(() =>
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument(),
    );
    expect(history.location.pathname).toBe("/work");
    expect(history.location.search).toBe("?period=week");
  });

  it("answers a question from the sheet's fixed footer", async () => {
    const user = userEvent.setup();
    start(`/work/${fx.question.id}`);
    const sheet = await screen.findByRole("dialog", {
      name: fx.question.title,
    });
    await user.selectOptions(
      await within(sheet).findByLabelText("Cargo weight"),
      "0",
    );
    await user.click(
      within(sheet).getByRole("button", { name: "Send answer" }),
    );
    expect(
      await within(sheet).findByText(
        "Answer sent. Your AI coworker carries on.",
      ),
    ).toBeInTheDocument();
    const call = api.calls.find(
      (entry) => entry.path === "/v1/work-blockers/b-1045/responses",
    );
    expect(call?.body).toEqual(
      expect.objectContaining({ response: { weight: "1.2t" } }),
    );
  });

  it("moves items between groups live", async () => {
    start("/work");
    const board = await screen.findByRole("region", { name: "Work" });
    const working = () =>
      within(board).getByRole("region", { name: "Working" });
    expect(within(working()).queryByText(fx.waiting.title)).toBeNull();
    const moved = api.items.find((entry) => entry.id === fx.waiting.id)!;
    moved.state = "in_progress";
    moved.wait = undefined;
    await act(async () => api.emit("wait_event_received", moved.id));
    await waitFor(() =>
      expect(within(working()).getByText(fx.waiting.title)).toBeInTheDocument(),
    );
  });

  it("says when it is reconnecting instead of pretending to be live", async () => {
    start("/work");
    await screen.findByRole("region", { name: "Work" });
    await waitFor(() =>
      expect(api.calls.some((c) => c.path === "/v1/work-events")).toBe(true),
    );
    await act(async () => api.drop());
    expect(await screen.findByText(/Reconnecting…/)).toBeInTheDocument();
  });
});

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
