import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setLocale } from "../i18n";
import * as fx from "../test/fixtures";
import { operator, type MockApi } from "../test/mockApi";
import { expectAccessible, startApp } from "../test/app";

let api: MockApi;
function start(...args: Parameters<typeof startApp>) {
  const started = startApp(...args);
  api = started.api;
  return started;
}
const a11y = expectAccessible;

/** Renders the phone board (below 900px) instead of the desktop one. */
function phone() {
  vi.stubGlobal(
    "matchMedia",
    vi.fn((query: string) => ({
      matches: false,
      media: query,
      addEventListener: () => {},
      removeEventListener: () => {},
    })),
  );
}

beforeEach(() => setLocale("en"));
afterEach(() => {
  vi.unstubAllGlobals();
  setLocale("en");
});

// Work, the Failed strip and the work item sheet (HOR-459).

describe("Work", () => {
  it("groups work and keeps waiting and paused work out of Needs you", async () => {
    start("/work");
    const board = await screen.findByRole("region", { name: "Work" });
    const column = (name: string) =>
      within(board).getByRole("region", { name });
    expect(within(column("Needs you")).getAllByRole("link")).toHaveLength(3);
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
      within(waitingColumn).getByText("nothing needed from you"),
    ).toBeInTheDocument();
    expect(
      within(column("Working")).getByText(fx.working.title),
    ).toBeInTheDocument();
    expect(within(column("Done")).getByText("today")).toBeInTheDocument();
    expect(
      screen.getByText(
        "Nothing leaves without your OK or your rules · Exact copies kept · Your data stays in your own environment",
      ),
    ).toBeInTheDocument();
    await a11y(screen.getByRole("main"));
  });

  it("shows a single failure as one bar, and says nothing was sent", async () => {
    start("/work");
    const strip = await screen.findByRole("region", { name: "Failed · 1" });
    expect(
      within(strip).getByText(`${fx.failed.title}: A step couldn't finish.`, {
        exact: false,
      }),
    ).toBeInTheDocument();
    expect(within(strip).getByText("Nothing was sent.")).toBeInTheDocument();
    expect(within(strip).queryByText(fx.paused.title)).not.toBeInTheDocument();
    expect(within(strip).getByRole("link", { name: "Review" })).toHaveAttribute(
      "href",
      `/work/${fx.failed.id}`,
    );
  });

  it("expands the Failed strip into reasons when several items failed", async () => {
    start("/work");
    await screen.findByRole("region", { name: "Failed · 1" });
    api.items.push({
      ...fx.failed,
      id: "W-2095",
      title: "Quote · Aveiro → Hamburg, 1×20′",
      workflowKey: fx.quotes.key,
      failureSummary: { code: "interrupted_after_write" },
    });
    await act(async () => api.emit("work_failed", "W-2095"));
    const strip = await screen.findByRole("region", { name: "Failed · 2" });
    expect(
      within(strip).getByText(
        "In the last 7 days. Nothing more happens on these until someone restarts them.",
      ),
    ).toBeInTheDocument();
    expect(
      within(strip).getByText("Interrupted after acting outside Iterabase"),
    ).toBeInTheDocument();
    expect(
      within(strip).getByRole("link", { name: "All 2 failed this month" }),
    ).toHaveAttribute("href", "/team");
    expect(
      within(strip).getByRole("link", { name: "Review restart" }),
    ).toHaveAttribute("href", `/work/${fx.failed.id}`);
    expect(within(strip).getByRole("link", { name: "Review" })).toHaveAttribute(
      "href",
      "/work/W-2095",
    );
  });

  it("folds the Failed reasons away and opens them again", async () => {
    const user = userEvent.setup();
    start("/work");
    await screen.findByRole("region", { name: "Failed · 1" });
    api.items.push({
      ...fx.failed,
      id: "W-2095",
      title: "Quote · Aveiro → Hamburg, 1×20′",
      failureSummary: { code: "interrupted_after_write" },
    });
    await act(async () => api.emit("work_failed", "W-2095"));
    const strip = await screen.findByRole("region", { name: "Failed · 2" });
    const hide = within(strip).getByRole("button", {
      name: "Hide failed work",
    });
    expect(hide).toHaveAttribute("aria-expanded", "true");
    await user.click(hide);
    expect(
      within(strip).queryByRole("link", { name: "Review restart" }),
    ).not.toBeInTheDocument();
    expect(
      within(strip).getByRole("link", { name: "All 2 failed this month" }),
    ).toBeInTheDocument();
    await user.click(
      within(strip).getByRole("button", { name: "Show failed work" }),
    );
    expect(
      within(strip).getByRole("link", { name: "Review restart" }),
    ).toBeInTheDocument();
  });

  it("closes a working item's timeline with the step it is on now", async () => {
    startApp(`/work/${fx.working.id}`);
    const sheet = await screen.findByRole("dialog", { name: fx.working.title });
    const timeline = within(sheet)
      .getByRole("heading", { name: "Timeline" })
      .closest("section")!;
    const rows = within(timeline).getAllByRole("listitem");
    expect(rows.at(-1)).toHaveTextContent(/^Now/);
  });

  it("opens an item in the side sheet and closes back to the board", async () => {
    const user = userEvent.setup();
    const { history } = start("/work?period=week");
    await user.click(
      await screen.findByRole("link", { name: new RegExp(fx.done.title) }),
    );
    const sheet = await screen.findByRole("dialog", { name: fx.done.title });
    expect(await within(sheet).findByText("Quote sent")).toBeInTheDocument();
    expect(within(sheet).getByText("Rita Mendes")).toBeInTheDocument();
    expect(within(sheet).getByText(fx.done.id)).toBeInTheDocument();
    expect(
      within(sheet).getByRole("heading", { name: "Timeline" }),
    ).toBeInTheDocument();
    expect(
      within(sheet).getByRole("heading", { name: "Sources and documents" }),
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

  it("reviews an approval in the sheet before approving it", async () => {
    const user = userEvent.setup();
    start(`/work/${fx.approval.id}`);
    const sheet = await screen.findByRole("dialog", {
      name: fx.approval.title,
    });
    const decision = await within(sheet).findByRole("region", {
      name: "Send this quote to Rui Costa",
    });
    expect(
      within(decision).getByText("compras@outroexemplo.pt"),
    ).toBeInTheDocument();
    expect(
      within(decision).getByText("Cotacao_W-1039.pdf"),
    ).toBeInTheDocument();
    expect(
      within(sheet).queryByRole("button", { name: "Approve & send" }),
    ).toBeNull();
    await a11y(sheet);
    // Enter mirrors the visible "Review and decide" button.
    expect(
      within(sheet).getByRole("button", { name: /Review and decide/ }),
    ).toBeInTheDocument();
    await user.keyboard("{Enter}");
    expect(decision).toHaveFocus();
    expect(
      within(sheet).getByRole("button", { name: "Not this one" }),
    ).toBeInTheDocument();
    await user.click(
      within(sheet).getByRole("button", { name: "Approve & send" }),
    );
    expect(
      await within(sheet).findByText(
        "Answer sent. Your AI coworker carries on.",
      ),
    ).toBeInTheDocument();
    const call = api.calls.find(
      (entry) => entry.path === "/v1/work-blockers/b-1039/responses",
    );
    expect(call?.body).toEqual(
      expect.objectContaining({ outcome: "approved" }),
    );
  });

  it("answers a one-choice question with the answer's own button", async () => {
    const user = userEvent.setup();
    start(`/work/${fx.question.id}`);
    const sheet = await screen.findByRole("dialog", {
      name: fx.question.title,
    });
    expect(await within(sheet).findByText("Cargo weight")).toBeInTheDocument();
    expect(within(sheet).queryByRole("combobox")).toBeNull();
    await user.click(within(sheet).getByRole("button", { name: "1.2 t" }));
    expect(
      await within(sheet).findByText(
        "Answer sent. Your AI coworker carries on.",
      ),
    ).toBeInTheDocument();
    const call = api.calls.find(
      (entry) => entry.path === "/v1/work-blockers/b-1045/responses",
    );
    expect(call?.body).toEqual(
      expect.objectContaining({
        outcome: "answered",
        response: { weight: "1.2t" },
      }),
    );
  });

  it("answers a question with a form field from the sheet's fixed footer", async () => {
    const user = userEvent.setup();
    start(`/work/${fx.login.id}`);
    const sheet = await screen.findByRole("dialog", { name: fx.login.title });
    const send = await within(sheet).findByRole("button", {
      name: "Send code",
    });
    expect(send).toBeDisabled();
    await user.type(within(sheet).getByLabelText("SMS code"), "481516");
    await user.click(send);
    await within(sheet).findByText("Answer sent. Your AI coworker carries on.");
    const call = api.calls.find(
      (entry) => entry.path === "/v1/work-blockers/b-2094/responses",
    );
    expect(call?.body).toEqual(
      expect.objectContaining({ response: { code: "481516" } }),
    );
  });

  it("shows why an item failed and restarts it with guidance", async () => {
    const user = userEvent.setup();
    start(`/work/${fx.failed.id}`);
    const sheet = await screen.findByRole("dialog", { name: fx.failed.title });
    expect(
      await within(sheet).findByRole("heading", {
        name: "A step couldn't finish",
      }),
    ).toBeInTheDocument();
    expect(
      within(sheet).getByText("Couldn't finish. Nothing was sent.", {
        exact: false,
      }),
    ).toBeInTheDocument();
    await a11y(sheet);
    await user.click(
      within(sheet).getByRole("button", { name: "Restart with guidance" }),
    );
    const dialog = await screen.findByRole("dialog", {
      name: "Restart with guidance",
    });
    expect(
      within(dialog).getByRole("button", { name: "Restart" }),
    ).toBeDisabled();
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

describe("Work on phone", () => {
  it("shows one group at a time behind filter chips", async () => {
    const user = userEvent.setup();
    phone();
    start("/work");
    const needs = await screen.findByRole("button", { name: "Needs you 3" });
    expect(needs).toHaveAttribute("aria-pressed", "true");
    const list = screen.getByRole("region", { name: "Needs you" });
    expect(within(list).getAllByRole("link")).toHaveLength(3);
    await user.click(screen.getByRole("button", { name: "Working 2" }));
    const working = screen.getByRole("region", { name: "Working" });
    expect(within(working).getByText(fx.working.title)).toBeInTheDocument();
    expect(screen.queryByText(fx.approval.title)).toBeNull();
    await a11y(screen.getByRole("main"));
  });

  it("opens the item full screen with a way back to Work", async () => {
    const user = userEvent.setup();
    phone();
    const { history } = start(`/work/${fx.approval.id}?period=month`);
    const sheet = await screen.findByRole("dialog", {
      name: fx.approval.title,
    });
    await user.click(
      within(sheet).getByRole("button", { name: "Back to Work" }),
    );
    await waitFor(() =>
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument(),
    );
    expect(history.location.pathname).toBe("/work");
    expect(history.location.search).toBe("?period=month");
  });
});

describe("Work in PT-PT", () => {
  it("uses the PT-PT board copy", async () => {
    setLocale("pt");
    start("/work", { ...operator, locale: "pt", localePreference: "pt" });
    expect(
      await screen.findByRole("heading", { name: "Todo o trabalho" }),
    ).toBeInTheDocument();
    expect(
      await screen.findByRole("region", { name: "Falhou · 1" }),
    ).toBeInTheDocument();
    expect(screen.getByText("nada precisa de si")).toBeInTheDocument();
  });
});
