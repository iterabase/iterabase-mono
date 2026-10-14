import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import axe from "axe-core";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import App from "./App";

const item = {
  id: "item-1",
  workflowKey: "walter/quotation",
  title: "Quotation request — ACME",
  currentAttemptId: "attempt-1",
  state: "done",
  source: {
    kind: "outlook",
    title: "Quote — ACME",
    subtitle: "buyer@acme.example",
    evidence: [{ label: { en: "Customer", pt: "Cliente" }, value: "ACME" }],
  },
  presentation: {
    workflowTitle: "Quotation processing",
    personaName: "Marco",
    locale: "en",
  },
  createdAt: "2026-08-05T09:00:00Z",
  updatedAt: "2026-08-05T09:05:00Z",
  finishedAt: "2026-08-05T09:05:00Z",
  valueConfigured: true,
  valueModel: {
    formula: "labor_time_saved",
    currency: "EUR",
    baselineSeconds: 1200,
    loadedHourlyCost: "30",
    assumptions: { source: "Customer baseline" },
    explanation: {
      en: "Customer assumptions",
      pt: "Pressupostos do cliente",
    },
  },
  estimatedValue: "10.00",
  valueCurrency: "EUR",
  valueDisputed: false,
};
const summary = {
  counts: { todo: 0, in_progress: 0, blocked: 0, done: 1, failed: 0 },
  value: {
    configured: true,
    estimated: true,
    totals: [{ amount: "10", currency: "EUR" }],
    models: [
      {
        formula: "labor_time_saved",
        currency: "EUR",
        baselineSeconds: 1200,
        loadedHourlyCost: "30",
        explanation: {
          en: "Customer assumptions",
          pt: "Pressupostos do cliente",
        },
      },
    ],
  },
  trend: [{ date: "2026-08-05", amount: "10", currency: "EUR" }],
};
const adminProfile = {
  id: "identity-1",
  email: "ada@example.com",
  displayName: "Ada Lovelace",
  role: "admin",
  locale: "en",
  updatedAt: "2026-09-23T10:00:00Z",
};
const operatorProfile = { ...adminProfile, role: "operator" };
let sessionProfile: typeof adminProfile = adminProfile;
function json(data: unknown, status = 200) {
  return Promise.resolve(
    new Response(JSON.stringify(data), {
      status,
      headers: { "Content-Type": "application/json" },
    }),
  );
}
function fetchMock(
  input: RequestInfo | URL,
  init?: RequestInit,
): Promise<Response> {
  const url = String(input);
  if (url === "/v1/auth/session")
    return json({
      authenticated: true,
      profile: sessionProfile,
      csrfToken: "csrf-1",
    });
  if (url === "/v1/profile")
    return json({ profile: sessionProfile, csrfToken: "csrf-2" });
  if (url === "/v1/auth/sign-out")
    return Promise.resolve(new Response(null, { status: 204 }));
  if (url.startsWith("/v1/work-events"))
    return new Promise((_resolve, reject) =>
      init?.signal?.addEventListener("abort", () =>
        reject(new DOMException("Aborted", "AbortError")),
      ),
    );
  if (url.startsWith("/v1/work-dashboard")) return json(summary);
  if (url.startsWith("/v1/work-items?")) return json([item]);
  if (url === "/v1/work-items/item-1") return json(item);
  if (url.endsWith("/attempts"))
    return json([
      {
        id: "attempt-1",
        workItemId: "item-1",
        number: 1,
        definitionKey: "walter/quotation",
        definitionVersion: "1",
        createdAt: item.createdAt,
        finishedAt: item.finishedAt,
      },
    ]);
  if (url.includes("/work-attempts/"))
    return json([
      {
        id: "node-1",
        attemptId: "attempt-1",
        nodeKey: "process",
        businessLabel: {
          en: "Processing quotation",
          pt: "A processar cotação",
        },
        resultPresentation: {
          outcomes: [
            {
              outcome: "completed",
              summary: {
                en: "Quotation processed",
                pt: "Pedido de cotação processado",
              },
            },
          ],
          fields: [
            {
              path: ["classification"],
              label: { en: "Classification", pt: "Classificação" },
              options: [
                {
                  value: "engineering",
                  label: { en: "Engineering", pt: "Engenharia" },
                },
                {
                  value: "pricing",
                  label: { en: "Pricing", pt: "Preços" },
                },
              ],
            },
            {
              path: ["customer"],
              label: { en: "Customer", pt: "Cliente" },
            },
            {
              path: ["customer", "name"],
              label: { en: "Name", pt: "Nome" },
            },
            {
              path: ["customer", "country"],
              label: { en: "Country", pt: "País" },
            },
            {
              path: ["quantity"],
              label: { en: "Quantity", pt: "Quantidade" },
            },
          ],
        },
        visit: 1,
        executionSeq: 1,
        kind: "agent_task",
        state: "succeeded",
        outcome: "completed",
        summary: "Quotation processed",
        output: {
          classification: "pricing",
          customer: { name: "ACME", country: "Portugal" },
          quantity: 1840.5,
        },
        createdAt: item.createdAt,
      },
    ]);
  if (url.endsWith("/timeline?limit=1000"))
    return json([
      {
        cursor: 1,
        id: "event-1",
        workItemId: "item-1",
        attemptId: "attempt-1",
        nodeExecutionId: "node-1",
        code: "node_completed",
        params: { summary: "Quotation processed" },
        createdAt: item.finishedAt,
      },
      {
        cursor: 2,
        id: "event-2",
        workItemId: "item-1",
        attemptId: "attempt-1",
        nodeExecutionId: "node-1",
        code: "work_completed",
        params: {},
        createdAt: item.finishedAt,
      },
    ]);
  if (url.endsWith("/blocker")) return json({ error: "not found" }, 404);
  if (url.endsWith("/feedback") && init?.method !== "POST") return json([]);
  if (url.endsWith("/feedback") && init?.method === "POST")
    return json(
      {
        id: "feedback-1",
        workItemId: "item-1",
        attemptId: "attempt-1",
        category: "poor_output",
        createdAt: new Date().toISOString(),
      },
      201,
    );
  if (url.endsWith("/revisions") && init?.method === "POST")
    return json({ ...item, state: "in_progress" }, 201);
  if (url.endsWith("/consequences")) return json([]);
  if (url.endsWith("/artifacts")) return json([]);
  throw new Error(`unexpected fetch ${url}`);
}

async function connect() {
  const user = userEvent.setup();
  render(<App />);
  await screen.findByText("Quotation request — ACME");
  return user;
}

function headersOf(init?: RequestInit): Record<string, string> {
  return (init?.headers || {}) as Record<string, string>;
}
function workCalls() {
  return vi
    .mocked(fetch)
    .mock.calls.filter(
      ([url]) =>
        !String(url).startsWith("/v1/auth/") && String(url) !== "/v1/profile",
    );
}
function unsafeWorkCall(fragment: string) {
  return workCalls().find(
    ([url, init]) => String(url).includes(fragment) && init?.method === "POST",
  );
}

describe("Platform v1 Dashboard", () => {
  beforeEach(() => {
    sessionProfile = adminProfile;
    vi.stubGlobal("fetch", vi.fn(fetchMock));
  });
  afterEach(() => vi.unstubAllGlobals());

  it("loads the board through the browser session cookie without any bearer credential", async () => {
    await connect();
    expect(
      screen.getByText("Estimated business value created"),
    ).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "To do" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Done" })).toBeInTheDocument();
    expect(screen.queryByText("Workflows")).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/API key/i)).not.toBeInTheDocument();
    await waitFor(() =>
      expect(
        workCalls().some(([url]) => String(url) === "/v1/work-events"),
      ).toBe(true),
    );
    const calls = workCalls();
    expect(calls.length).toBeGreaterThan(0);
    for (const [, init] of calls) {
      expect(init?.credentials).toBe("same-origin");
      expect(headersOf(init)).not.toHaveProperty("Authorization");
    }
  });

  it("never offers an API-key entry: an anonymous visitor gets the sign-in surface", async () => {
    vi.mocked(fetch).mockImplementation((input, init) =>
      String(input) === "/v1/auth/session"
        ? json({ authenticated: false })
        : fetchMock(input, init),
    );
    render(<App />);
    expect(
      await screen.findByRole("button", { name: "Sign in" }),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Work email")).toBeInTheDocument();
    expect(screen.queryByLabelText(/API key/i)).not.toBeInTheDocument();
    expect(
      vi
        .mocked(fetch)
        .mock.calls.some(([url]) => String(url).startsWith("/v1/work-")),
    ).toBe(false);
  });

  it("shows the unavailable surface when browser sign-in is not configured", async () => {
    vi.mocked(fetch).mockImplementation((input, init) =>
      String(input) === "/v1/auth/session"
        ? json({ error: "unavailable", code: "auth_unavailable" }, 503)
        : fetchMock(input, init),
    );
    render(<App />);
    expect(
      await screen.findByRole("heading", {
        name: "Sign-in is temporarily unavailable",
      }),
    ).toBeInTheDocument();
    expect(screen.queryByLabelText(/API key/i)).not.toBeInTheDocument();
  });

  it("returns to sign-in when a work call reports the session expired", async () => {
    vi.mocked(fetch).mockImplementation((input, init) =>
      String(input).startsWith("/v1/work-dashboard")
        ? json(
            {
              error: "Your session expired. Sign in again to continue.",
              code: "session_expired",
            },
            401,
          )
        : fetchMock(input, init),
    );
    render(<App />);
    expect(
      await screen.findByRole("button", { name: "Sign in" }),
    ).toBeInTheDocument();
    expect(
      screen.getByText("Your session expired. Sign in again to continue."),
    ).toBeInTheDocument();
  });

  it("returns to sign-in when the live event stream reports the session expired", async () => {
    vi.mocked(fetch).mockImplementation((input, init) =>
      String(input) === "/v1/work-events"
        ? json({ error: "expired", code: "session_expired" }, 401)
        : fetchMock(input, init),
    );
    render(<App />);
    expect(
      await screen.findByRole("button", { name: "Sign in" }),
    ).toBeInTheDocument();
  });

  it("renders an Operator board whose value fields were clipped by the server", async () => {
    sessionProfile = operatorProfile;
    const clipped = {
      ...item,
      valueConfigured: false,
      valueModel: undefined,
      estimatedValue: undefined,
      valueCurrency: undefined,
      valueDisputed: false,
    };
    vi.mocked(fetch).mockImplementation((input, init) => {
      const url = String(input);
      if (url.startsWith("/v1/work-dashboard"))
        return json({
          ...summary,
          value: {
            configured: false,
            estimated: false,
            totals: [],
            models: [],
          },
          trend: [],
        });
      if (url.startsWith("/v1/work-items?")) return json([clipped]);
      if (url === "/v1/work-items/item-1") return json(clipped);
      return fetchMock(input, init);
    });
    const user = await connect();
    expect(screen.getByText("Value model not configured")).toBeInTheDocument();
    expect(
      screen.getByText("No value recorded for this period"),
    ).toBeInTheDocument();
    expect(screen.queryByText(/€/)).not.toBeInTheDocument();
    await user.click(screen.getByText("Quotation request — ACME"));
    expect(
      await screen.findByRole("heading", { name: "Quotation processed" }),
    ).toBeInTheDocument();
    expect(screen.queryByText(/€/)).not.toBeInTheDocument();
  });

  it("refreshes a stale CSRF proof once and retries the unsafe call", async () => {
    let feedbackPosts = 0;
    vi.mocked(fetch).mockImplementation((input, init) => {
      if (String(input).endsWith("/feedback") && init?.method === "POST") {
        feedbackPosts += 1;
        if (headersOf(init)["X-CSRF-Token"] === "csrf-1")
          return json({ error: "stale", code: "csrf" }, 403);
      }
      return fetchMock(input, init);
    });
    const user = await connect();
    await user.click(screen.getByText("Quotation request — ACME"));
    await screen.findByRole("heading", { name: "Quotation processed" });
    await user.click(
      screen.getByRole("button", { name: /Result needs improvement/ }),
    );
    await user.click(screen.getByRole("button", { name: "Save feedback" }));
    await waitFor(() =>
      expect(screen.getByText(/without starting new work/)).toBeInTheDocument(),
    );
    expect(feedbackPosts).toBe(2);
    const posts = workCalls().filter(
      ([url, init]) =>
        String(url).endsWith("/feedback") && init?.method === "POST",
    );
    expect(posts.map(([, init]) => headersOf(init)["X-CSRF-Token"])).toEqual([
      "csrf-1",
      "csrf-2",
    ]);
  });

  it("moves between the board and account security, and signs out with the CSRF proof", async () => {
    const user = await connect();
    await user.click(screen.getByRole("button", { name: "Account" }));
    expect(
      await screen.findByRole("heading", { name: "Profile" }),
    ).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Work" }));
    expect(
      await screen.findByText("Quotation request — ACME"),
    ).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Sign out" }));
    expect(
      await screen.findByRole("button", { name: "Sign in" }),
    ).toBeInTheDocument();
    const signOut = vi
      .mocked(fetch)
      .mock.calls.find(([url]) => String(url) === "/v1/auth/sign-out");
    expect(headersOf(signOut?.[1])["X-CSRF-Token"]).toBe("csrf-1");
  });

  it("keeps a saved Profile language when returning to the board (HOR-670)", async () => {
    vi.mocked(fetch).mockImplementation((input, init) => {
      if (String(input) === "/v1/profile" && init?.method === "PATCH") {
        sessionProfile = { ...adminProfile, locale: "pt" };
        return json({ profile: sessionProfile });
      }
      return fetchMock(input, init);
    });
    const user = await connect();
    await user.click(screen.getByRole("button", { name: "Account" }));
    await screen.findByRole("heading", { name: "Profile" });
    await user.selectOptions(screen.getByLabelText("Language"), "pt");
    await user.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByRole("status")).toHaveTextContent(
      "Alterações guardadas.",
    );
    await user.click(screen.getByRole("button", { name: "Trabalho" }));
    expect(
      await screen.findByRole("heading", { name: "Concluído" }),
    ).toBeInTheDocument();
    expect(document.documentElement.lang).toBe("pt-PT");
    await user.click(screen.getByRole("button", { name: "Conta" }));
    expect(
      await screen.findByRole("heading", { name: "Perfil" }),
    ).toBeInTheDocument();
  });

  it("follows a changed organisation default on the board when the Admin follows it (HOR-670)", async () => {
    sessionProfile = {
      ...adminProfile,
      localePreference: "",
      organizationLocale: "",
    } as typeof adminProfile;
    vi.mocked(fetch).mockImplementation((input, init) => {
      const url = String(input);
      if (url === "/v1/organization/settings") {
        if (init?.method === "PATCH") {
          sessionProfile = {
            ...sessionProfile,
            locale: "pt",
            organizationLocale: "pt",
          } as typeof adminProfile;
          return json({
            defaultLocale: "pt",
            updatedAt: "2026-09-23T12:00:00Z",
          });
        }
        return json({ defaultLocale: "", updatedAt: "2026-09-23T10:00:00Z" });
      }
      if (url === "/v1/access-requests") return json([]);
      return fetchMock(input, init);
    });
    const user = await connect();
    await user.click(screen.getByRole("button", { name: "Account" }));
    await user.click(
      await screen.findByRole("button", { name: "Access requests" }),
    );
    await user.selectOptions(
      await screen.findByLabelText("Default language"),
      "pt",
    );
    await user.click(
      screen.getByRole("button", { name: "Save default language" }),
    );
    expect(await screen.findByRole("status")).toHaveTextContent(
      "Idioma predefinido guardado.",
    );
    await user.click(screen.getByRole("button", { name: "Trabalho" }));
    expect(
      await screen.findByRole("heading", { name: "Concluído" }),
    ).toBeInTheDocument();
  });

  function signOutCalls() {
    return vi
      .mocked(fetch)
      .mock.calls.filter(([url]) => String(url) === "/v1/auth/sign-out")
      .map(([, init]) => headersOf(init)["X-CSRF-Token"]);
  }
  function staleSignOutProof(input: RequestInfo | URL, init?: RequestInit) {
    if (
      String(input) === "/v1/auth/sign-out" &&
      headersOf(init)["X-CSRF-Token"] === "csrf-1"
    )
      return json({ error: "stale", code: "csrf" }, 403);
    return fetchMock(input, init);
  }

  it("refreshes a stale CSRF proof before signing out from the board", async () => {
    vi.mocked(fetch).mockImplementation(staleSignOutProof);
    const user = await connect();
    await user.click(screen.getByRole("button", { name: "Sign out" }));
    expect(
      await screen.findByRole("button", { name: "Sign in" }),
    ).toBeInTheDocument();
    expect(signOutCalls()).toEqual(["csrf-1", "csrf-2"]);
  });

  it("refreshes a stale CSRF proof before signing out from Account", async () => {
    vi.mocked(fetch).mockImplementation(staleSignOutProof);
    const user = await connect();
    await user.click(screen.getByRole("button", { name: "Account" }));
    await screen.findByRole("heading", { name: "Profile" });
    await user.click(
      screen.getAllByRole("button", { name: "Sign out this device" })[0],
    );
    expect(
      await screen.findByRole("button", { name: "Sign in" }),
    ).toBeInTheDocument();
    expect(signOutCalls()).toEqual(["csrf-1", "csrf-2"]);
  });

  it("stays signed in and says so when sign-out fails", async () => {
    vi.mocked(fetch).mockImplementation((input, init) =>
      String(input) === "/v1/auth/sign-out"
        ? json({ error: "unavailable", code: "auth_unavailable" }, 503)
        : fetchMock(input, init),
    );
    const user = await connect();
    await user.click(screen.getByRole("button", { name: "Sign out" }));
    expect(
      await screen.findByText(/Could not sign out\. You are still signed in/),
    ).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Sign in" })).toBeNull();
    expect(screen.getByText("Quotation request — ACME")).toBeInTheDocument();
  });

  it("keeps active business steps visible and failed work outside the four columns when value is unconfigured", async () => {
    const active = {
      ...item,
      id: "item-active",
      state: "in_progress",
      finishedAt: undefined,
      startedAt: "2026-08-05T09:01:00Z",
      estimatedValue: undefined,
      valueConfigured: false,
      currentStep: {
        key: "process",
        label: {
          en: "Checking quotation details",
          pt: "A verificar detalhes da cotação",
        },
        state: "running",
      },
    };
    const failed = {
      ...item,
      id: "item-failed",
      title: "Quotation request — Could not parse",
      state: "failed",
      estimatedValue: undefined,
      valueConfigured: false,
      failureSummary: { message: "The quotation could not be processed." },
    };
    const unconfigured = {
      ...summary,
      counts: { todo: 0, in_progress: 1, blocked: 0, done: 0, failed: 1 },
      value: { configured: false, estimated: false, totals: [], models: [] },
      trend: [],
    };
    vi.mocked(fetch).mockImplementation((input, init) => {
      const url = String(input);
      if (url.startsWith("/v1/work-dashboard")) return json(unconfigured);
      if (url.startsWith("/v1/work-items?")) return json([active, failed]);
      if (url === "/v1/work-items/item-active") return json(active);
      return fetchMock(input, init);
    });
    const user = await connect();
    expect(screen.getByText("Value model not configured")).toBeInTheDocument();
    expect(screen.getByText("Checking quotation details")).toBeInTheDocument();
    expect(
      screen.getByRole("heading", { name: /Could not complete/ }),
    ).toBeInTheDocument();
    expect(
      screen.getByText("Quotation request — Could not parse"),
    ).toBeInTheDocument();
    await user.click(screen.getByText("Quotation request — ACME"));
    expect(
      (await screen.findAllByText("Processing quotation")).length,
    ).toBeGreaterThan(0);
    expect(
      screen.queryByText(
        /model messages|worker details|tool calls?|system prompts?/i,
      ),
    ).not.toBeInTheDocument();
  });

  it("shows waiting and paused work as active, never Blocked, and stops a wait on request", async () => {
    const waiting = {
      ...item,
      state: "waiting",
      finishedAt: undefined,
      startedAt: "2026-08-05T09:01:00Z",
      estimatedValue: undefined,
      wait: {
        completion: "all",
        expected: 3,
        received: 2,
        deadlineAt: "2026-08-08T09:01:00Z",
      },
    };
    const paused = {
      ...item,
      id: "item-paused",
      title: "Quotation request — Paused",
      state: "paused",
      finishedAt: undefined,
      estimatedValue: undefined,
      pause: { reason: "runner_offline", since: "2026-08-05T09:02:00Z" },
    };
    let stopped = false;
    vi.mocked(fetch).mockImplementation((input, init) => {
      const url = String(input);
      if (url.startsWith("/v1/work-items?")) return json([waiting, paused]);
      if (url === "/v1/work-items/item-1")
        return json(stopped ? { ...waiting, state: "in_progress" } : waiting);
      if (url.endsWith("/wait/stop") && init?.method === "POST") {
        stopped = true;
        return json({ ...waiting, state: "in_progress" });
      }
      return fetchMock(input, init);
    });
    const user = await connect();
    const inProgress = screen
      .getByRole("heading", { name: "In progress" })
      .closest(".column") as HTMLElement;
    expect(
      within(inProgress).getByText("Quotation request — ACME"),
    ).toBeInTheDocument();
    expect(within(inProgress).getByText("2 of 3 received")).toBeInTheDocument();
    expect(
      within(inProgress).getByText(
        "A connected system is offline. Work resumes automatically when it reconnects.",
      ),
    ).toBeInTheDocument();
    const blocked = screen
      .getByRole("heading", { name: "Blocked" })
      .closest(".column") as HTMLElement;
    expect(within(blocked).getByText("No items")).toBeInTheDocument();

    await user.click(screen.getByText("Quotation request — ACME"));
    expect(
      await screen.findByRole("heading", { name: "Waiting for responses" }),
    ).toBeInTheDocument();
    await user.click(
      screen.getByRole("button", { name: "Stop waiting and continue" }),
    );
    expect(await screen.findByRole("status")).toHaveTextContent(
      "Stopped waiting. Work continues with what arrived.",
    );
    expect(
      vi
        .mocked(fetch)
        .mock.calls.some(
          ([url, init]) =>
            String(url) === "/v1/work-items/item-1/wait/stop" &&
            init?.method === "POST" &&
            headersOf(init)["X-CSRF-Token"] === "csrf-1",
        ),
    ).toBe(true);
  });

  it("opens the board in the signed-in person's language", async () => {
    sessionProfile = { ...adminProfile, locale: "pt" };
    await connect();
    expect(screen.getByText("Painel")).toBeInTheDocument();
    expect(
      screen.getByRole("heading", { name: "Concluído" }),
    ).toBeInTheDocument();
  });

  it("inspects a completed outcome and saves feedback without starting a revision", async () => {
    const user = await connect();
    await user.click(screen.getByText("Quotation request — ACME"));
    expect(
      await screen.findByRole("heading", { name: "Quotation processed" }),
    ).toBeInTheDocument();
    expect(screen.getByText("Pricing")).toBeInTheDocument();
    expect(screen.getByText("1,840.5")).toBeInTheDocument();
    const itemValueExplanation = screen
      .getAllByText("How this is estimated")
      .at(-1);
    expect(itemValueExplanation).toBeDefined();
    await user.click(itemValueExplanation!);
    expect(screen.getByText("Customer assumptions")).toBeInTheDocument();
    expect(screen.getByText("Manual handling time saved")).toBeInTheDocument();
    // Whole amounts drop the decimals (HOR-670): €30, 30 €.
    expect(screen.getByText(/min × €30\/hour/)).toBeInTheDocument();
    expect(screen.getByText("Customer baseline")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "PT" }));
    expect(
      screen.getByText("Tempo de trabalho manual poupado"),
    ).toBeInTheDocument();
    expect(screen.getByText(/min × 30 €\/hora/)).toBeInTheDocument();
    expect(screen.getByText("Classificação")).toBeInTheDocument();
    expect(screen.getByText("Preços")).toBeInTheDocument();
    // Typed numeric results follow the locale (HOR-670).
    expect(screen.getByText("1.840,5")).toBeInTheDocument();
    expect(screen.getAllByText("Cliente").length).toBeGreaterThan(0);
    expect(screen.getByText("Nome")).toBeInTheDocument();
    expect(screen.getAllByText("ACME").length).toBeGreaterThan(0);
    expect(
      screen.getByRole("heading", { name: "Pedido de cotação processado" }),
    ).toBeInTheDocument();
    expect(screen.queryByText("classification")).not.toBeInTheDocument();
    expect(screen.queryByText("pricing")).not.toBeInTheDocument();
    expect(screen.queryByText("Quotation processed")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "EN" }));
    expect(
      screen.queryByText(/successful|correct|approved/i),
    ).not.toBeInTheDocument();
    await user.click(
      screen.getByRole("button", { name: /Result needs improvement/ }),
    );
    await user.click(screen.getByRole("button", { name: "Poor output" }));
    await user.type(
      screen.getByPlaceholderText(/Explanation/),
      "Wrong extracted total",
    );
    await user.click(screen.getByRole("button", { name: "Save feedback" }));
    await waitFor(() =>
      expect(screen.getByText(/without starting new work/)).toBeInTheDocument(),
    );
    const post = vi
      .mocked(fetch)
      .mock.calls.find(
        ([url, init]) =>
          String(url).endsWith("/feedback") && init?.method === "POST",
      );
    expect(post).toBeTruthy();
    expect(headersOf(post?.[1])["X-CSRF-Token"]).toBe("csrf-1");
    expect(
      vi
        .mocked(fetch)
        .mock.calls.some(([url]) => String(url).endsWith("/revisions")),
    ).toBe(false);
  });

  it("does not expose output without immutable field presentation", async () => {
    vi.mocked(fetch).mockImplementation((input, init) => {
      const url = String(input);
      if (url.includes("/work-attempts/") && url.endsWith("/nodes"))
        return json([
          {
            id: "node-unpresented",
            attemptId: "attempt-1",
            nodeKey: "process",
            businessLabel: {
              en: "Processing quotation",
              pt: "A processar cotação",
            },
            resultPresentation: {
              outcomes: [
                {
                  outcome: "completed",
                  summary: {
                    en: "Quotation processed",
                    pt: "Pedido de cotação processado",
                  },
                },
              ],
            },
            visit: 1,
            executionSeq: 1,
            kind: "agent_task",
            state: "succeeded",
            outcome: "completed",
            output: {
              customer: "ACME",
              amount: 100,
              records: [{ customer: "Northstar", amount: 50 }],
              prompt: "private model instruction",
            },
            createdAt: item.createdAt,
          },
        ]);
      return fetchMock(input, init);
    });
    const user = await connect();
    await user.click(screen.getByText("Quotation request — ACME"));
    const summaryHeading = await screen.findByRole("heading", {
      name: "Quotation processed",
    });
    const outcome = summaryHeading.closest("section");
    expect(outcome).not.toBeNull();
    const result = within(outcome!);
    expect(
      result.getByText("No business details recorded."),
    ).toBeInTheDocument();
    expect(result.queryByText("ACME")).not.toBeInTheDocument();
    expect(result.queryByText("Northstar")).not.toBeInTheDocument();
    expect(
      result.queryByText("private model instruction"),
    ).not.toBeInTheDocument();
  });

  it("requires exact consequence confirmation before starting a revised attempt", async () => {
    const consequence = {
      invocationId: "invocation-1",
      summary: {
        en: "Send the quotation email again",
        pt: "Enviar novamente o email da cotação",
      },
      state: "succeeded",
    };
    vi.mocked(fetch).mockImplementation((input, init) =>
      String(input).endsWith("/consequences")
        ? json([consequence])
        : fetchMock(input, init),
    );
    const user = await connect();
    await user.click(screen.getByText("Quotation request — ACME"));
    await screen.findByRole("heading", { name: "Quotation processed" });
    await user.click(
      screen.getByRole("button", { name: /Result needs improvement/ }),
    );
    await user.click(
      screen.getByRole("button", { name: "Try again with feedback" }),
    );
    await user.type(
      screen.getByPlaceholderText(/Actionable guidance/),
      "Use the corrected classification rule",
    );
    const start = screen.getByRole("button", { name: "Start revised attempt" });
    expect(start).toBeDisabled();
    await user.click(screen.getByText("Send the quotation email again"));
    expect(start).toBeEnabled();
    await user.click(start);
    await waitFor(() =>
      expect(
        screen.getByText(/original history is preserved/),
      ).toBeInTheDocument(),
    );
    const revision = vi
      .mocked(fetch)
      .mock.calls.find(
        ([url, init]) =>
          String(url).endsWith("/revisions") && init?.method === "POST",
      );
    expect(revision?.[1]?.body).toContain("invocation-1");
    expect(headersOf(revision?.[1])["X-CSRF-Token"]).toBe("csrf-1");
    expect(headersOf(revision?.[1])).not.toHaveProperty("Authorization");
  });

  it("uploads and attaches a file when resolving an artifact blocker", async () => {
    const blocked = {
      ...item,
      id: "item-blocked",
      currentAttemptId: "attempt-blocked",
      state: "blocked",
      blocker: {
        id: "blocker-1",
        kind: "artifact",
        title: {
          en: "Signed agreement required",
          pt: "Acordo assinado necessário",
        },
      },
    };
    const blocker = {
      id: "blocker-1",
      workItemId: blocked.id,
      attemptId: blocked.currentAttemptId,
      kind: "artifact",
      title: blocked.blocker.title,
      description: {
        en: "Upload the signed agreement",
        pt: "Carregue o acordo assinado",
      },
      responseSchema: {
        type: "object",
        properties: { note: { type: "string", title: "Note" } },
      },
      allowedOutcomes: ["provided"],
      responsePresentation: {
        outcomes: [{ en: "Provide", pt: "Fornecer" }],
        fields: [{ key: "note", label: { en: "Note", pt: "Nota" } }],
      },
      state: "open",
    };
    vi.mocked(fetch).mockImplementation((input, init) => {
      const url = String(input);
      if (url.startsWith("/v1/work-items?")) return json([blocked]);
      if (url === "/v1/work-items/item-blocked") return json(blocked);
      if (url.includes("attempt-blocked") && url.includes("/nodes"))
        return json([]);
      if (url.endsWith("/blocker")) return json(blocker);
      if (url === "/v1/artifacts" && init?.method === "POST")
        return json({ artifactId: "artifact-1" }, 201);
      if (url.includes("/work-blockers/blocker-1/responses"))
        return json({ ...blocker, state: "resolved" });
      return fetchMock(input, init);
    });
    const user = await connect();
    await user.click(screen.getByText("Quotation request — ACME"));
    expect(
      await screen.findByRole("heading", { name: "Signed agreement required" }),
    ).toBeInTheDocument();
    const file = new File(["signed"], "agreement.pdf", {
      type: "application/pdf",
    });
    await user.upload(
      screen.getByLabelText("Choose the required artifact"),
      file,
    );
    const send = screen.getByRole("button", { name: "Send response" });
    await waitFor(() => expect(send).toBeEnabled());
    await user.click(send);
    await waitFor(() =>
      expect(
        vi
          .mocked(fetch)
          .mock.calls.some(([url]) =>
            String(url).includes("/work-blockers/blocker-1/responses"),
          ),
      ).toBe(true),
    );
    const response = vi
      .mocked(fetch)
      .mock.calls.find(([url]) =>
        String(url).includes("/work-blockers/blocker-1/responses"),
      );
    expect(response?.[1]?.body).toContain("artifact-1");
    expect(headersOf(response?.[1])["X-CSRF-Token"]).toBe("csrf-1");
    const upload = unsafeWorkCall("/v1/artifacts");
    expect(upload?.[1]?.credentials).toBe("same-origin");
    expect(headersOf(upload?.[1])["X-CSRF-Token"]).toBe("csrf-1");
    expect(headersOf(upload?.[1])["Content-Type"]).toBe("application/pdf");
  });

  it("renders a done item whose result node exposes fields/artifacts but no outcomes", async () => {
    // A result node may carry a resultPresentation with fields + artifactRefs
    // but no `outcomes` array (the workflow contract makes outcomes optional).
    // The detail render must not crash and white-screen the app.
    vi.mocked(fetch).mockImplementation((input, init) => {
      const url = String(input);
      if (url.includes("/work-attempts/"))
        return json([
          {
            id: "node-1",
            attemptId: "attempt-1",
            nodeKey: "process",
            businessLabel: { en: "Processing quotation", pt: "A processar" },
            resultPresentation: {
              fields: [{ path: ["classification"], label: { en: "Class" } }],
            },
            artifactRefs: [{ artifactId: "art-1", role: "output" }],
            visit: 1,
            executionSeq: 1,
            kind: "agent_task",
            state: "succeeded",
            outcome: "completed",
            output: { classification: "pricing" },
            createdAt: item.createdAt,
          },
        ]);
      if (url.endsWith("/artifacts"))
        return json([
          {
            artifactId: "art-1",
            attemptId: "attempt-1",
            role: "output",
            metadata: { name: "quote.pdf" },
            mimeType: "application/pdf",
            sizeBytes: 1234,
            createdAt: item.finishedAt,
          },
        ]);
      return fetchMock(input, init);
    });
    const user = await connect();
    await user.click(screen.getByText("Quotation request — ACME"));
    // Would white-screen (root unmounted) before the fix; the artifact row must appear.
    expect(await screen.findByText("quote.pdf")).toBeInTheDocument();
    expect(screen.getByText(/Download/)).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("contains a detail render error in the panel instead of white-screening the app", async () => {
    // The backend always returns an array here, but if any render error slips
    // through (e.g. malformed detail payload), the ErrorBoundary must keep the
    // dashboard usable instead of unmounting the whole React root.
    vi.mocked(fetch).mockImplementation((input, init) => {
      const url = String(input);
      if (url.endsWith("/artifacts")) return json(null);
      return fetchMock(input, init);
    });
    const user = await connect();
    await user.click(screen.getByText("Quotation request — ACME"));
    expect(
      await screen.findByText("Something went wrong opening this item."),
    ).toBeInTheDocument();
    // PRD §3 "Business language only": the raw exception text must NOT leak
    // into the customer-facing panel (it goes to the operator-only channel).
    expect(
      screen.queryByText(/Cannot read|TypeError|undefined|is not a function/i),
    ).not.toBeInTheDocument();
    // Dashboard is still alive (not a white-screen / unmounted root).
    expect(screen.getByRole("heading", { name: "Done" })).toBeInTheDocument();
    // The panel can be closed to return to the board.
    await user.click(screen.getByRole("button", { name: "Close" }));
    expect(
      screen.queryByText("Something went wrong opening this item."),
    ).not.toBeInTheDocument();
  });

  it("keeps focus through a board refresh and submits correctly typed JSON-Schema fields", async () => {
    let dashboardCalls = 0;
    let completeRefresh: (() => Promise<void>) | undefined;
    const blocked = {
      ...item,
      id: "item-decision",
      currentAttemptId: "attempt-decision",
      state: "blocked",
      source: { ...item.source, kind: "legacy_crm" },
      blocker: {
        id: "blocker-decision",
        kind: "decision",
        title: { en: "Choose the delivery path", pt: "Escolha a entrega" },
      },
    };
    const blocker = {
      id: "blocker-decision",
      workItemId: blocked.id,
      attemptId: blocked.currentAttemptId,
      kind: "decision",
      title: blocked.blocker.title,
      description: {
        en: "Choose and supply the decision details.",
        pt: "Escolha e forneça os detalhes da decisão.",
      },
      responseSchema: {
        type: "object",
        required: ["amount", "quantity", "details", "items", "route"],
        properties: {
          amount: { type: "number", title: "Amount" },
          quantity: { type: "integer", title: "Quantity" },
          details: { type: "object", title: "Details" },
          items: { type: "array", title: "Items" },
          route: {
            type: "string",
            title: "Routing strategy",
            enum: ["claims_queue", "operations_queue"],
          },
        },
      },
      allowedOutcomes: ["approved", "rejected"],
      responsePresentation: {
        outcomes: [
          { en: "Authorize", pt: "Autorizar" },
          { en: "Decline", pt: "Recusar" },
        ],
        fields: [
          {
            key: "amount",
            label: { en: "Proposed amount", pt: "Montante proposto" },
          },
          {
            key: "quantity",
            label: { en: "Item count", pt: "Número de itens" },
          },
          {
            key: "details",
            label: { en: "Business details", pt: "Detalhes de negócio" },
          },
          {
            key: "items",
            label: { en: "Included items", pt: "Itens incluídos" },
          },
          {
            key: "route",
            label: { en: "Destination", pt: "Destino" },
            options: [
              { en: "Send to claims", pt: "Encaminhar para reclamações" },
              { en: "Send to operations", pt: "Encaminhar para operações" },
            ],
          },
        ],
      },
      state: "open",
    };
    vi.mocked(fetch).mockImplementation((input, init) => {
      const url = String(input);
      if (url.startsWith("/v1/work-items?")) return json([blocked]);
      // Hold the locale-triggered refresh until typing is in progress; the
      // regression must not depend on where its 180 ms debounce happens to land.
      if (url.startsWith("/v1/work-dashboard") && ++dashboardCalls === 2)
        return new Promise<Response>((resolve) => {
          completeRefresh = async () => resolve(await json(summary));
        });
      if (url === "/v1/work-items/item-decision") return json(blocked);
      if (url.endsWith("/attempts")) return json([]);
      if (url.endsWith("/blocker")) return json(blocker);
      if (url.includes("/work-blockers/blocker-decision/responses"))
        return json({ ...blocker, state: "resolved" });
      return fetchMock(input, init);
    });
    const user = await connect();
    await user.click(screen.getByRole("button", { name: "PT" }));
    await user.click(screen.getByText("Quotation request — ACME"));
    await screen.findByRole("heading", { name: "Escolha a entrega" });
    expect(screen.getByRole("dialog")).toHaveFocus();
    const outcome = screen.getByLabelText("Decisão solicitada");
    expect(
      screen.getByRole("option", { name: "Autorizar" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "Recusar" })).toBeInTheDocument();
    expect(screen.getAllByText("Origem externa").length).toBeGreaterThan(0);
    expect(
      screen.queryByText("legacy crm", { exact: false }),
    ).not.toBeInTheDocument();
    await user.selectOptions(outcome, "rejected");
    await waitFor(() => expect(completeRefresh).toBeDefined());
    const amount = screen.getByLabelText("Montante proposto");
    await user.type(amount, "12");
    expect(amount).toHaveFocus();
    await act(async () => {
      await completeRefresh!();
    });
    expect(amount).toHaveFocus();
    // Continue typing without clicking again: a refresh must not steal focus.
    await user.keyboard(".5");
    expect(amount).toHaveValue("12.5");
    await user.type(screen.getByLabelText("Número de itens"), "3");
    await user.click(screen.getByLabelText("Detalhes de negócio"));
    await user.paste('{"priority":"high"}');
    await user.click(screen.getByLabelText("Itens incluídos"));
    await user.paste('["quote"]');
    await user.selectOptions(screen.getByLabelText("Destino"), "0");
    expect(
      screen.getByRole("option", { name: "Encaminhar para reclamações" }),
    ).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Enviar resposta" }));
    await waitFor(() =>
      expect(
        vi
          .mocked(fetch)
          .mock.calls.some(([url]) =>
            String(url).includes("/work-blockers/blocker-decision/responses"),
          ),
      ).toBe(true),
    );
    const response = vi
      .mocked(fetch)
      .mock.calls.find(([url]) =>
        String(url).includes("/work-blockers/blocker-decision/responses"),
      );
    const body = JSON.parse(String(response?.[1]?.body));
    expect(body.outcome).toBe("rejected");
    expect(body.response).toEqual({
      amount: 12.5,
      quantity: 3,
      details: { priority: "high" },
      items: ["quote"],
      route: "claims_queue",
    });
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("requires exact confirmation before resolving a consequence blocker and localizes it in Portuguese", async () => {
    const blocked = {
      ...item,
      id: "item-confirmation",
      currentAttemptId: "attempt-confirmation",
      state: "blocked",
      source: { ...item.source, kind: "schedule" },
      blocker: {
        id: "blocker-confirmation",
        kind: "consequence_confirmation",
        title: { en: "Confirm repeated action", pt: "Confirmar ação repetida" },
      },
    };
    const blocker = {
      id: "blocker-confirmation",
      workItemId: blocked.id,
      attemptId: blocked.currentAttemptId,
      kind: "consequence_confirmation",
      title: blocked.blocker.title,
      description: {
        en: "Review the possible action.",
        pt: "Reveja a ação possível.",
      },
      responseSchema: { type: "object" },
      allowedOutcomes: ["confirmed"],
      responsePresentation: {
        outcomes: [{ en: "Confirm", pt: "Confirmar" }],
      },
      requiredConsequences: [
        {
          invocationId: "invocation-1",
          summary: {
            en: "Send the quotation email again",
            pt: "Enviar novamente o email da cotação",
          },
          state: "succeeded",
        },
      ],
      state: "open",
    };
    vi.mocked(fetch).mockImplementation((input, init) => {
      const url = String(input);
      if (url.startsWith("/v1/work-items?")) return json([blocked]);
      if (url === "/v1/work-items/item-confirmation") return json(blocked);
      if (url.endsWith("/attempts")) return json([]);
      if (url.endsWith("/blocker")) return json(blocker);
      if (url.includes("/work-blockers/blocker-confirmation/responses"))
        return json({ ...blocker, state: "resolved" });
      return fetchMock(input, init);
    });
    const user = await connect();
    await user.click(screen.getByRole("button", { name: "PT" }));
    await user.click(screen.getByText("Quotation request — ACME"));
    expect(
      await screen.findAllByText("Confirmação de ação consequente"),
    ).toHaveLength(2);
    expect(screen.getAllByText("Agendado").length).toBeGreaterThan(0);
    const send = screen.getByRole("button", { name: "Enviar resposta" });
    expect(send).toBeDisabled();
    await user.click(screen.getByText("Enviar novamente o email da cotação"));
    expect(send).toBeEnabled();
    await user.click(send);
    const response = await waitFor(() => {
      const call = vi
        .mocked(fetch)
        .mock.calls.find(([url]) =>
          String(url).includes("/work-blockers/blocker-confirmation/responses"),
        );
      expect(call).toBeTruthy();
      return call;
    });
    const body = JSON.parse(String(response?.[1]?.body));
    expect(body.confirmedInvocationIds).toEqual(["invocation-1"]);
  });

  it("has no automatically detectable accessibility violations on the Dashboard", async () => {
    await connect();
    const results = await axe.run(document.body, {
      rules: { "color-contrast": { enabled: false } },
    });
    expect(results.violations).toEqual([]);
  });
});
