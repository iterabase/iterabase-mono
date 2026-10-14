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

// Work, the Failed strip and the work item sheet (HOR-459).

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
