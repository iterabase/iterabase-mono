import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setLocale } from "../i18n";
import * as fx from "../test/fixtures";
import { operator } from "../test/mockApi";
import { expectAccessible, startApp } from "../test/app";

beforeEach(() => setLocale("en"));
afterEach(() => {
  vi.unstubAllGlobals();
  setLocale("en");
});

const quotesPath = `/team/${encodeURIComponent(fx.quotes.key)}`;
const ratesPath = `/team/${encodeURIComponent(fx.rates.key)}`;

// Team and AI coworker detail (HOR-459, boards XTTeam and XTCoworker*).

describe("Team", () => {
  it("shows each AI coworker from its workflow definition", async () => {
    startApp("/team");
    const card = await screen.findByRole("article", { name: "Quotes" });
    expect(
      within(card).getByText("Sales desk · AI coworker"),
    ).toBeInTheDocument();
    expect(
      within(card).getByText("Reads customer emails, PDFs and handwriting"),
    ).toBeInTheDocument();
    // "Always asks you" comes from the human gates, never declared copy.
    expect(
      within(card).getByText("Before any email to a customer"),
    ).toBeInTheDocument();
    expect(screen.getByRole("article", { name: "Rates" })).toBeInTheDocument();
    expect(
      screen.getByText(/^Two AI coworkers working in your mailbox/),
    ).toBeInTheDocument();
  });

  it("says when a new installation has no AI coworkers yet", async () => {
    startApp("/team", operator, { empty: true });
    expect(
      await screen.findByRole("heading", { name: "No AI coworkers yet" }),
    ).toBeInTheDocument();
    await expectAccessible();
  });

  it("keeps the page gutters for a coworker that does not exist", async () => {
    startApp("/team/nobody");
    const title = await screen.findByRole("heading", { name: "Nothing here" });
    expect(title.closest(".state-page")).not.toBeNull();
  });

  it("shows the week in real counts and the estimated hours back", async () => {
    startApp("/team");
    const card = await screen.findByRole("article", { name: "Quotes" });
    // 6000 seconds back for Quotes in the dashboard: ≈ 1.7 h, an estimate.
    expect(within(card).getAllByText("≈ 1.7 h").length).toBeGreaterThan(0);
    expect(screen.getByText("handled this week")).toBeInTheDocument();
  });

  it("offers a restart when a coworker's work failed", async () => {
    startApp("/team");
    const card = await screen.findByRole("article", { name: "Rates" });
    expect(
      within(card).getAllByText(`${fx.failed.title} couldn’t finish.`).length,
    ).toBeGreaterThan(0);
    expect(
      within(card).getByRole("link", { name: /Restart with guidance/ }),
    ).toHaveAttribute("href", `/work/${fx.failed.id}`);
  });

  it("opens a coworker from its card", async () => {
    const user = userEvent.setup();
    startApp("/team");
    await user.click(await screen.findByRole("link", { name: "Open Rates" }));
    expect(
      await screen.findByRole("heading", { level: 1, name: "Rates" }),
    ).toBeInTheDocument();
  });

  it("has no accessibility violations", async () => {
    startApp("/team");
    await screen.findByRole("article", { name: "Quotes" });
    await expectAccessible(screen.getByRole("main"));
  });

  it("renders in European Portuguese", async () => {
    startApp("/team", { ...operator, locale: "pt", localePreference: "pt" });
    expect(
      await screen.findByRole("heading", { level: 1, name: "A sua equipa" }),
    ).toBeInTheDocument();
    const card = await screen.findByRole("article", { name: "Quotes" });
    expect(within(card).getByText("Sozinho")).toBeInTheDocument();
    expect(within(card).getByText("Pergunta-lhe sempre")).toBeInTheDocument();
  });
});

describe("AI coworker", () => {
  it("opens a coworker's How it works, What it can do and Activity", async () => {
    const user = userEvent.setup();
    startApp(quotesPath);
    expect(
      await screen.findByRole("heading", { level: 1, name: "Quotes" }),
    ).toBeInTheDocument();
    const flow = screen.getByRole("list", {
      name: "How Quotes works, step by step",
    });
    expect(within(flow).getByText("Collects replies")).toBeInTheDocument();
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

  it("counts live items at each step of How it works", async () => {
    startApp(quotesPath);
    const flow = await screen.findByRole("list", {
      name: "How Quotes works, step by step",
    });
    const steps = within(flow).getAllByRole("listitem");
    expect(steps).toHaveLength(fx.quotes.steps.length);
    // W-1039 waits on "You confirm the quote"; W-1051 collects replies.
    await waitFor(() =>
      expect(steps[6]).toHaveTextContent("You confirm the quote1 needs you"),
    );
    expect(steps[4]).toHaveTextContent("Collects replies1 waiting");
  });

  it("types what it does on its own, with details and approved values (DES-HOR-459-09)", async () => {
    startApp(`${quotesPath}?tab=can`);
    const own = await screen.findByRole("region", { name: /On its own/ });
    const sends = within(own)
      .getByText("Asks your 6 usual suppliers for rates")
      .closest("li")!;
    expect(sends).toHaveTextContent("Sends");
    expect(sends).toHaveTextContent(
      "Pre-approved recipients · sent without review",
    );
    expect(
      within(
        within(sends).getByRole("list", { name: /^Values for:/ }),
      ).getAllByRole("listitem"),
    ).toHaveLength(6);
    expect(
      within(own)
        .getByText("Reads customer emails, PDFs and handwriting")
        .closest("li"),
    ).toHaveTextContent("Reads");
  });

  it("shows who confirmed each item in Activity (DES-HOR-459-10)", async () => {
    startApp(`${quotesPath}?tab=activity&period=week`);
    const table = await screen.findByRole("table");
    expect(
      within(table).getByRole("columnheader", { name: "Confirmed by" }),
    ).toBeInTheDocument();
    const row = within(table).getByText("Rita Mendes").closest("tr")!;
    expect(row).toHaveTextContent(fx.done.title);
  });

  it("marks what it asks you by the gate's kind and splits connections", async () => {
    startApp(`${quotesPath}?tab=can`);
    const asks = await screen.findByRole("region", {
      name: /Always asks you first/,
    });
    expect(
      within(asks).getByText("Before any email to a customer").closest("li"),
    ).toHaveTextContent("You confirm");
    expect(
      within(asks)
        .getByText("When a detail of the request is unclear")
        .closest("li"),
    ).toHaveTextContent("You answer");
    const connections = screen.getByRole("region", {
      name: "Connections it uses",
    });
    expect(
      within(connections).getByText("Microsoft 365 mailbox"),
    ).toBeInTheDocument();
    expect(
      within(connections).getByText("quotes@exemplo.pt"),
    ).toBeInTheDocument();
  });

  it("shows failures with what happens next under the Failed filter", async () => {
    const { api } = startApp(`${ratesPath}?tab=activity&period=month`);
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string, init?: RequestInit) =>
        String(url).includes(`/work-items/${fx.failed.id}/consequences`)
          ? Promise.resolve(
              new Response(JSON.stringify([{ state: "outcome_unknown" }]), {
                headers: { "content-type": "application/json" },
              }),
            )
          : api.handle(url, init ?? {}),
      ),
    );
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: /Failed/ }));
    const table = await screen.findByRole("table");
    expect(
      within(table).getByRole("columnheader", { name: "Why it failed" }),
    ).toBeInTheDocument();
    expect(
      within(table).getByText("A step couldn't finish"),
    ).toBeInTheDocument();
    expect(within(table).getByText("Not restarted")).toBeInTheDocument();
    // The outcome of an earlier action is unknown: review before restarting.
    expect(
      await within(table).findByText("Outcome unknown"),
    ).toBeInTheDocument();
    expect(
      within(table).getByRole("link", { name: `Review ${fx.failed.title}` }),
    ).toHaveAttribute("href", `/work/${fx.failed.id}`);
  });

  it("keeps the old Failed this month link working", async () => {
    startApp(`${ratesPath}?tab=failed`);
    expect(
      await screen.findByRole("button", { name: /Failed/, pressed: true }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "This month", pressed: true }),
    ).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Activity" })).toHaveAttribute(
      "aria-current",
      "page",
    );
  });

  it("says when a coworker is not found", async () => {
    startApp("/team/default%2Fnobody");
    expect(
      await screen.findByRole("heading", { level: 1 }),
    ).not.toHaveTextContent("Quotes");
  });

  it.each(["how", "can", "activity"])(
    "has no accessibility violations on %s",
    async (tab) => {
      startApp(`${quotesPath}?tab=${tab}`);
      await screen.findByRole("heading", { level: 1, name: "Quotes" });
      await expectAccessible(screen.getByRole("main"));
    },
  );

  it("renders in European Portuguese", async () => {
    startApp(quotesPath, { ...operator, locale: "pt", localePreference: "pt" });
    expect(
      await screen.findByRole("link", { name: "Como funciona" }),
    ).toHaveAttribute("aria-current", "page");
    expect(screen.getByText("Ativo agora")).toBeInTheDocument();
    expect(
      screen.getByRole("navigation", { name: "Secções do colaborador" }),
    ).toBeInTheDocument();
  });
});
