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
    expect(within(yourTurn).getByText("3")).toBeInTheDocument();
    expect(
      within(yourTurn).getByText("decisions · about 2 minutes"),
    ).toBeInTheDocument();
    expect(
      within(yourTurn).getByRole("link", { name: /Start with the first one/ }),
    ).toHaveAttribute("href", "/your-turn");
    // The day in one sentence: counts the API has, never money.
    const sentence = screen.getByText(/^Since yesterday/);
    expect(sentence).toHaveTextContent(
      "Since yesterday Quotes finished 2 items. 3 decisions are waiting for you, about 2 minutes in total.",
    );
    expect(sentence.textContent).not.toMatch(/€/);
    // The team right now, from the workflow definitions; a failed coworker
    // says what failed.
    const team = screen.getByRole("region", { name: "Your team right now" });
    expect(within(team).getByText("Quotes")).toBeInTheDocument();
    expect(within(team).getByText("working")).toBeInTheDocument();
    expect(within(team).getByText("Rates")).toBeInTheDocument();
    expect(within(team).getByText("failed")).toBeInTheDocument();
    expect(
      within(team).getByRole("link", { name: "Open Team" }),
    ).toHaveAttribute("href", "/team");
    // Waiting for you lists only human actions, oldest first.
    const waitingForYou = screen.getByRole("region", {
      name: "Waiting for you",
    });
    expect(
      within(waitingForYou)
        .getAllByRole("link")
        .map((row) => row.textContent),
    ).toEqual([
      expect.stringContaining("SMS code sent to your phone"),
      expect.stringContaining("Which weight: 1.2 t or 7.2 t?"),
      expect.stringContaining("Send this quote to Rui Costa"),
    ]);
    expect(within(waitingForYou).getAllByRole("link")[2]).toHaveAttribute(
      "href",
      `/work/${fx.approval.id}`,
    );
    const done = screen.getByRole("region", { name: "Done since yesterday" });
    expect(within(done).getByText(fx.done.title)).toBeInTheDocument();
    // The status bar: time back, the failed pill, the trust line.
    await waitFor(() =>
      expect(
        screen.getAllByText(
          (_, element) =>
            element?.tagName === "SPAN" &&
            element.textContent ===
              "You got 1 h 50 back so far today (estimate)",
        ).length,
      ).toBeGreaterThan(0),
    );
    expect(screen.getByRole("link", { name: /^Failed · 1/ })).toHaveAttribute(
      "href",
      "/work",
    );
    expect(
      screen.getAllByText(/Nothing leaves without your OK/).length,
    ).toBeGreaterThan(0);
  });

  it("starts Your turn with Enter", async () => {
    const user = userEvent.setup();
    const { history } = start("/");
    await screen.findByRole("region", { name: "Waiting for you" });
    await user.keyboard("{Enter}");
    await waitFor(() => expect(history.location.pathname).toBe("/your-turn"));
  });

  it("says when nothing needs you", async () => {
    start("/");
    const blocked = api.items.filter((item) => item.state === "blocked");
    for (const item of blocked) api.items.splice(api.items.indexOf(item), 1);
    const yourTurn = await screen.findByRole("region", { name: "Your turn" });
    expect(
      within(yourTurn).getByText("Nothing needs you right now."),
    ).toBeInTheDocument();
    expect(
      within(yourTurn).queryByRole("link", { name: /Start with/ }),
    ).not.toBeInTheDocument();
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
    // The frame's phone header sits outside landmarks (Shell-owned).
    await a11y(screen.getByRole("main"));
  });

  it("renders in European Portuguese", async () => {
    start("/", { ...operator, locale: "pt", localePreference: "pt" });
    expect(
      await screen.findByRole("heading", { name: "A sua vez" }),
    ).toBeInTheDocument();
    const nav = screen.getByRole("navigation", { name: "Principal" });
    expect(within(nav).getByRole("link", { name: "Hoje" })).toBeInTheDocument();
    expect(screen.getByText(/^Desde ontem/)).toHaveTextContent(
      "Desde ontem, o Quotes concluiu 2 itens. 3 decisões estão à sua espera, cerca de 2 minutos no total.",
    );
    await waitFor(() =>
      expect(
        screen.getAllByText(
          (_, element) =>
            element?.tagName === "SPAN" &&
            element.textContent ===
              "Já tem 1 h 50 poupadas até agora (estimativa)",
        ).length,
      ).toBeGreaterThan(0),
    );
  });
});

describe("Your turn", () => {
  it("clears decisions one at a time and ends in All clear on Today", async () => {
    const user = userEvent.setup();
    const { history } = start("/your-turn");
    // Oldest first: the login code, the question, then the approval.
    expect(await screen.findByText("Decision 1 of 3")).toBeInTheDocument();
    expect(
      await screen.findByRole("heading", {
        level: 1,
        name: "SMS code sent to your phone",
      }),
    ).toBeInTheDocument();
    // Up next shows what follows.
    expect(
      screen.getByRole("heading", { name: "Up next" }),
    ).toBeInTheDocument();
    const sendCode = screen.getByRole("button", { name: /Send code/ });
    expect(sendCode).toBeDisabled();
    await user.type(screen.getByLabelText("SMS code"), "123456");
    await user.click(sendCode);

    expect(await screen.findByText("Decision 2 of 3")).toBeInTheDocument();
    expect(
      await screen.findByRole("heading", {
        level: 1,
        name: "Which weight: 1.2 t or 7.2 t?",
      }),
    ).toBeInTheDocument();
    // What was cleared shows as Done, answered by the person.
    expect(screen.getByRole("heading", { name: "Done" })).toBeInTheDocument();
    expect(
      screen.getByText(/^Answered by Rita · \d\d:\d\d$/),
    ).toBeInTheDocument();
    const send = screen.getByRole("button", { name: /Send answer/ });
    expect(send).toBeDisabled();
    await user.selectOptions(screen.getByLabelText("Cargo weight"), "1");
    await user.click(send);

    expect(await screen.findByText("Decision 3 of 3")).toBeInTheDocument();
    expect(
      await screen.findByRole("heading", {
        level: 1,
        name: "Send this quote to Rui Costa",
      }),
    ).toBeInTheDocument();
    // The exact content and its provenance are in front of the person.
    expect(
      screen.getByRole("region", { name: "Exact content" }),
    ).toHaveTextContent("RE: Pedido de cotação LCL Leixões–Santos");
    expect(screen.getByText(/^Prepared by/).closest("p")).toHaveTextContent(
      /Prepared by Quotes · \d\d:\d\d · Picks the best rate/,
    );
    // How it was built: the producing step's presented fields.
    const built = screen.getByRole("region", { name: "How it was built" });
    expect(within(built).getByText("Subject")).toBeInTheDocument();
    expect(
      screen.getByRole("region", { name: "What Quotes did" }),
    ).toHaveTextContent("Picks the best rate");
    // A, E and N mirror visible buttons.
    expect(
      screen.getByRole("button", { name: /Approve & send/ }),
    ).toBeEnabled();
    expect(screen.getByRole("button", { name: /Open the item/ })).toBeEnabled();
    expect(screen.getByRole("button", { name: /Not this one/ })).toBeEnabled();
    await user.keyboard("a");

    expect(
      await screen.findByRole("heading", {
        name: "That’s everything for now.",
      }),
    ).toBeInTheDocument();
    // All clear shows in the frame, with Today current.
    expect(history.location.pathname).toBe("/");
    const nav = screen.getByRole("navigation", { name: "Main" });
    expect(within(nav).getByRole("link", { name: "Today" })).toHaveAttribute(
      "aria-current",
      "page",
    );
    expect(screen.getAllByText("Answered by Rita")).toHaveLength(3);
    expect(
      screen.getByText(/^3 decisions · \d+ min \d+ s of your time$/),
    ).toBeInTheDocument();
    // The frame's phone header sits outside landmarks (Shell-owned).
    await a11y(screen.getByRole("main"));

    const responses = api.calls.filter((call) =>
      call.path.startsWith("/v1/work-blockers/"),
    );
    expect(responses.map((call) => [call.path, call.body])).toEqual([
      [
        "/v1/work-blockers/b-2094/responses",
        expect.objectContaining({
          outcome: "entered",
          response: { code: "123456" },
        }),
      ],
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

    // Back to Today leaves All clear.
    await user.click(screen.getByRole("link", { name: "Back to Today" }));
    expect(
      await screen.findByRole("heading", { level: 1, name: /, Rita\.$/ }),
    ).toBeInTheDocument();
  });

  it("skips a decision for later and says it still waits", async () => {
    const user = userEvent.setup();
    start("/your-turn");
    const skip = () =>
      user.click(
        screen.getByRole("button", { name: "Not now, show the next one" }),
      );
    await screen.findByText("Decision 1 of 3");
    await screen.findByRole("button", { name: "Not now, show the next one" });
    await skip();
    await screen.findByText("Decision 1 of 2");
    await screen.findByRole("heading", { level: 1, name: /Which weight/ });
    await skip();
    await screen.findByText("Decision 1 of 1");
    await screen.findByRole("heading", { level: 1, name: /Send this quote/ });
    await skip();
    expect(
      await screen.findByText("3 decisions are still waiting for you on Work."),
    ).toBeInTheDocument();
    expect(api.calls.some((call) => call.method === "POST")).toBe(false);
  });

  it("goes back to Today with Escape", async () => {
    const user = userEvent.setup();
    const { history } = start("/your-turn");
    await screen.findByText("Decision 1 of 3");
    await user.keyboard("{Escape}");
    await waitFor(() => expect(history.location.pathname).toBe("/"));
  });

  it("has no accessibility violations", async () => {
    start("/your-turn");
    await screen.findByRole("heading", {
      level: 1,
      name: "SMS code sent to your phone",
    });
    await a11y();
  });

  it("renders in European Portuguese", async () => {
    start("/your-turn", { ...operator, locale: "pt", localePreference: "pt" });
    expect(await screen.findByText("Decisão 1 de 3")).toBeInTheDocument();
    expect(
      screen.getByRole("link", { name: /Voltar a Hoje/ }),
    ).toBeInTheDocument();
    expect(screen.getByText("Cópia exata guardada")).toBeInTheDocument();
  });
});

describe("Pseudo-locale", () => {
  it("leaves no unlocalized copy on Today", async () => {
    setPseudoLocale(true);
    try {
      start("/");
      await screen.findByText(fx.done.title);
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
