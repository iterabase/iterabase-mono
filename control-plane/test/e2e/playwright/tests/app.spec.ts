import { readFileSync } from "node:fs";
import AxeBuilder from "@axe-core/playwright";
import { type Page } from "@playwright/test";
import { requiredEnv } from "../env";
import { expect, test, type BrowserObservability } from "./fixtures";

// The V2 customer app (HOR-459) against a deployed control plane: sign-in,
// Today, Your turn to All clear, Work and the work item sheet, live
// reconnection and the phone layout, in English and European Portuguese.

const email = requiredEnv("ITERABASE_BROWSER_EMAIL");
const password = requiredEnv("ITERABASE_BROWSER_PASSWORD");
const coordinator = requiredEnv("ITERABASE_BROWSER_COORDINATOR");
const doneTitle = requiredEnv("ITERABASE_BROWSER_DONE_TITLE");
const approvalTitle = requiredEnv("ITERABASE_BROWSER_APPROVAL_TITLE");
const uploadTitle = requiredEnv("ITERABASE_BROWSER_UPLOAD_TITLE");
const reconnectTitle = requiredEnv("ITERABASE_BROWSER_RECONNECT_TITLE");
const downloadName = requiredEnv("ITERABASE_BROWSER_DOWNLOAD_NAME");
const downloadBody = requiredEnv("ITERABASE_BROWSER_DOWNLOAD_BODY");

const greeting = /^Good (morning|afternoon|evening), .+\.$/;
// The ported All clear heading uses a typographic apostrophe.
const allClearHeading = /^That[’']s everything for now\.$/;

// The browser session lives only in the HttpOnly cookie: page script can
// read no cookie, and storage holds nothing but the router's scroll
// positions (DES-HOR-459-05) - never a credential or work data.
const scrollRestorationKey = /^tsr-scroll-restoration-v\d+_\d+$/;

async function assertFreshBrowserState(page: Page): Promise<void> {
  await expect
    .poll(() =>
      page.evaluate(() => ({
        cookie: document.cookie,
        local: Object.keys(localStorage),
        session: Object.keys(sessionStorage),
      })),
    )
    .toEqual({ cookie: "", local: [], session: expect.any(Array) });
  const session = await page.evaluate(() => Object.keys(sessionStorage));
  for (const key of session) expect(key).toMatch(scrollRestorationKey);
}

// signIn reaches the app only through the browser session: the HttpOnly
// session cookie is invisible to page script and nothing is kept in browser
// storage.
async function signIn(page: Page, secret = password): Promise<void> {
  await page.getByLabel("Email", { exact: true }).fill(email);
  await page.getByLabel("Password", { exact: true }).fill(secret);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
}

async function connect(page: Page, path = "/"): Promise<void> {
  await page.goto(path);
  await assertFreshBrowserState(page);
  const field = page.getByLabel("Email", { exact: true });
  await field.focus();
  await expect(field).toBeFocused();
  await signIn(page);
  await assertFreshBrowserState(page);
}

async function openWork(page: Page): Promise<void> {
  await page
    .getByRole("navigation", { name: "Main" })
    .getByRole("link", { name: /^Work/ })
    .click();
  await expect(
    page.getByRole("region", { name: "Work", exact: true }),
  ).toBeVisible();
}

async function setLanguage(page: Page, value: "en" | "pt"): Promise<void> {
  await page.goto("/settings/profile");
  const language = page.getByRole("combobox", { name: /^(Language|Idioma)/ });
  await language.selectOption(value);
  await page
    .getByRole("button", { name: /^(Save changes|Guardar alterações)$/ })
    .click();
  await expect(
    page.getByText(/^(Changes saved\.|Alterações guardadas\.)$/),
  ).toBeVisible();
}

function allowNetworkOutage(observability: BrowserObservability): void {
  observability.allowHTTPStatus(
    502,
    /^\/v1\//,
    "HOR-483 intentionally interrupts the Go-owned verified API forward to exercise customer error and SSE reconnect states.",
  );
  observability.allowRequestFailure(
    /^\/v1\//,
    /^net::(ERR_ABORTED|ERR_INCOMPLETE_CHUNKED_ENCODING)$/,
    "HOR-483 intentionally interrupts active API/SSE requests before Go restores the same deployed fixture.",
  );
  observability.allowConsoleError(
    /Failed to load resource: (net::ERR_INCOMPLETE_CHUNKED_ENCODING|the server responded with a status of 502)/,
    "Chromium reports the same explicitly allowlisted HOR-483 API/SSE outage in its console channel.",
  );
}

test("lands on Today through a browser session, in both languages, and signs out", async ({
  page,
  observability,
}) => {
  observability.allowHTTPStatus(
    401,
    /^\/v1\/auth\/sign-in$/,
    "The wrong-password customer error state deliberately exercises the deployed sign-in rejection.",
  );
  observability.allowConsoleError(
    /Failed to load resource: the server responded with a status of 401/,
    "Chromium reports the same explicitly allowlisted deployed sign-in rejection in its console channel.",
  );
  observability.allowRequestFailure(
    /^\/v1\/auth\/sign-out$/,
    /^net::ERR_ABORTED$/,
    "Chromium reports a fetch answered 204 No Content as net::ERR_ABORTED although the fetch resolves; the journey asserts the signed-out state and the reload proves the server session ended.",
  );
  await page.goto("/");
  await assertFreshBrowserState(page);
  await expect(page.getByLabel("Work API key")).toHaveCount(0);

  await signIn(page, "not-the-operator-password");
  await expect(page.getByRole("alert")).toHaveText(
    "We could not sign you in with those details.",
  );

  await signIn(page);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(greeting);
  await expect(
    page.getByRole("region", { name: "Your turn", exact: true }),
  ).toBeVisible();
  await expect(
    page
      .getByRole("region", { name: "Waiting for you", exact: true })
      .getByText(approvalTitle),
  ).toBeVisible();
  // Sofia and Reports arrive with HOR-452 / HOR-658 (DES-HOR-459-03).
  await expect(
    page.getByRole("navigation", { name: "Main" }).getByRole("link"),
  ).toHaveText(["Today", /^Work/, "Team"]);
  await assertFreshBrowserState(page);

  // European Portuguese, chosen in Profile, across Today and the work item.
  await setLanguage(page, "pt");
  await page.getByRole("link", { name: "Hoje" }).first().click();
  await expect(page.getByRole("heading", { name: "A sua vez" })).toBeVisible();
  await page
    .getByRole("navigation", { name: "Principal" })
    .getByRole("link", { name: /^Trabalho/ })
    .click();
  await expect(
    page.getByRole("region", { name: "Trabalho", exact: true }),
  ).toBeVisible();
  await page.getByRole("link", { name: new RegExp(doneTitle) }).click();
  const portugueseDetail = page.getByRole("dialog", { name: doneTitle });
  await expect(
    portugueseDetail.getByRole("heading", { name: "Revisão concluída" }),
  ).toBeVisible();
  await expect(
    portugueseDetail.getByRole("heading", { name: "Cronologia" }),
  ).toBeVisible();
  await expect(
    portugueseDetail.getByRole("button", {
      name: new RegExp(`Transferir.*${downloadName}`),
    }),
  ).toBeVisible();
  await page.keyboard.press("Escape");
  await setLanguage(page, "en");

  await page.getByRole("button", { name: /account and Settings$/ }).click();
  await page.getByRole("menuitem", { name: "Sign out" }).click();
  await expect(
    page.getByRole("button", { name: /^(Sign in|Entrar)$/ }),
  ).toBeVisible();
  await assertFreshBrowserState(page);

  // Signing out ended the server session: a reload returns to sign-in.
  await page.reload();
  await expect(
    page.getByRole("button", { name: /^(Sign in|Entrar)$/ }),
  ).toBeVisible();
  await expect(page.getByRole("navigation", { name: "Main" })).toHaveCount(0);
});

test("covers Work, search, the customer-safe item sheet, download, keyboard and accessibility", async ({
  page,
}) => {
  await connect(page);
  await openWork(page);
  const needsYou = page
    .getByRole("region", { name: "Work", exact: true })
    .getByRole("region", { name: "Needs you", exact: true });
  await expect(
    needsYou.getByText(approvalTitle, { exact: true }),
  ).toBeVisible();
  await expect(needsYou.getByText(uploadTitle, { exact: true })).toBeVisible();

  // ⌘K search across work items and coworkers.
  await page.keyboard.press("ControlOrMeta+k");
  const search = page.getByRole("dialog", { name: "Search" });
  await search
    .getByRole("searchbox", { name: "Search" })
    .fill("no matching synthetic case");
  await expect(search.getByText("Nothing matches that.")).toBeVisible();
  await search.getByRole("searchbox", { name: "Search" }).fill(doneTitle);
  await search.getByRole("button", { name: new RegExp(doneTitle) }).click();

  const detail = page.getByRole("dialog", { name: doneTitle });
  await expect(detail).toBeVisible();
  await expect(
    detail.getByRole("heading", { name: "Review completed" }),
  ).toBeVisible();
  await expect(detail.getByRole("heading", { name: "Timeline" })).toBeVisible();
  await expect(detail).not.toContainText("PRIVATE-SOURCE-MUST-NOT-LEAK-478");
  await expect(detail).not.toContainText(/prompt|worker-id|raw tool trace/i);

  const downloadPromise = page.waitForEvent("download");
  await detail.getByRole("button", { name: new RegExp(downloadName) }).click();
  const download = await downloadPromise;
  expect(download.suggestedFilename()).toBe(downloadName);
  const downloadedPath = await download.path();
  expect(downloadedPath).not.toBeNull();
  expect(readFileSync(downloadedPath!, "utf8")).toBe(downloadBody);

  await page.keyboard.press("Escape");
  await expect(detail).toHaveCount(0);
  await expect(page).toHaveURL(/\/work(\?.*)?$/);

  // A deep link opens the same sheet after a reload.
  await needsYou.getByText(approvalTitle, { exact: true }).click();
  await expect(page.getByRole("dialog", { name: approvalTitle })).toBeVisible();
  await page.reload();
  await expect(page.getByRole("dialog", { name: approvalTitle })).toBeVisible();
  await page.keyboard.press("Escape");

  test.info().annotations.push({
    type: "accessibility-baseline",
    description:
      "WCAG 2.0/2.1 A+AA automated structural rules; color contrast stays with the Design System token baseline (--ink-500 is the lightest text) because deterministic visual contrast is not represented by this browser fixture.",
  });
  for (const path of ["/", "/work", "/team"]) {
    await page.goto(path);
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    const accessibility = await new AxeBuilder({ page })
      .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
      .disableRules(["color-contrast"])
      .analyze();
    expect(accessibility.violations, path).toEqual([]);
  }
});

test("clears decisions in Your turn, reaches All clear and records feedback through real APIs", async ({
  page,
}) => {
  await connect(page);

  await openWork(page);
  await page.getByRole("link", { name: new RegExp(doneTitle) }).click();
  const done = page.getByRole("dialog", { name: doneTitle });
  await done.getByRole("button", { name: "Something was wrong" }).click();
  await done.getByRole("button", { name: "Poor result" }).click();
  await done
    .getByLabel("What should it have done? (optional)")
    .fill("Browser-reviewed synthetic correction.");
  await done.getByRole("button", { name: "Save feedback" }).click();
  await expect(done.getByRole("status")).toContainText(
    "Feedback kept with this item. Nothing new was started.",
  );
  await page.keyboard.press("Escape");

  await page
    .getByRole("navigation", { name: "Main" })
    .getByRole("link", { name: "Today" })
    .click();
  await page.getByRole("link", { name: /Start with the first one/ }).click();

  // Clear this journey's two decisions one at a time; decisions other stages
  // of the deployed scenario left open are skipped with "Not now".
  const cleared = new Set<string>();
  const allClear = page.getByRole("heading", { name: allClearHeading });
  while (!(await allClear.isVisible())) {
    await expect(
      page.getByText(/^Decision \d+ of \d+$/).or(allClear),
    ).toBeVisible();
    if (await allClear.isVisible()) break;
    const progress = await page
      .getByText(/^Decision \d+ of \d+$/)
      .textContent();
    const card = page.getByRole("article");
    await expect(card.getByRole("heading", { level: 1 })).toBeVisible();
    if (await card.getByText(approvalTitle, { exact: false }).isVisible()) {
      await expect(
        card.getByRole("heading", { level: 1, name: "Review required" }),
      ).toBeVisible();
      // The Design System checkbox hides the native input behind its label,
      // which is what a person clicks.
      await card.locator("label", { hasText: "Approved" }).click();
      await expect(card.getByLabel("Approved")).toBeChecked();
      await card.getByRole("button", { name: /^Accepted/ }).click();
      cleared.add(approvalTitle);
    } else if (
      await card.getByText(uploadTitle, { exact: false }).isVisible()
    ) {
      await expect(
        card.getByRole("heading", { level: 1, name: "Evidence file required" }),
      ).toBeVisible();
      await card.getByLabel("Choose the file to send").setInputFiles({
        name: "uploaded-browser-evidence.txt",
        mimeType: "text/plain",
        buffer: Buffer.from("Synthetic Playwright upload for HOR-483.\n"),
      });
      await card
        .getByLabel("Note")
        .fill("Reviewed in the real browser journey.");
      await card.getByRole("button", { name: /^Provided/ }).click();
      cleared.add(uploadTitle);
    } else {
      await card
        .getByRole("button", { name: "Not now, show the next one" })
        .click();
    }
    // Answering or skipping always moves the progress on (or ends it).
    await expect(page.getByText(progress ?? "", { exact: true })).toHaveCount(
      0,
    );
  }
  expect([...cleared].sort()).toEqual([approvalTitle, uploadTitle].sort());

  await expect(allClear).toBeVisible();
  await page.getByRole("link", { name: "See all work" }).click();
  const board = page.getByRole("region", { name: "Work", exact: true });
  await expect(
    board
      .getByRole("region", { name: "Done", exact: true })
      .getByText(approvalTitle),
  ).toBeVisible();
  await expect(
    board
      .getByRole("region", { name: "Done", exact: true })
      .getByText(uploadTitle),
  ).toBeVisible();
});

test("says it is reconnecting during a Go-owned network break and shows new work after", async ({
  page,
  request,
  observability,
}) => {
  await connect(page);
  await openWork(page);
  allowNetworkOutage(observability);

  const restart = await request.post(`${coordinator}/restart`);
  expect(restart.status()).toBe(202);
  await expect
    .poll(
      async () =>
        (await (await request.get(`${coordinator}/status`)).json()).status,
    )
    .toBe("outage");
  await expect(page.getByText(/^Reconnecting…/)).toBeVisible({
    timeout: 30_000,
  });

  const recover = await request.post(`${coordinator}/recover`);
  expect(recover.status()).toBe(202);
  await expect
    .poll(
      async () =>
        (await (await request.get(`${coordinator}/status`)).json()).status,
      { timeout: 30_000 },
    )
    .toBe("ready");
  await expect(page.getByText(reconnectTitle, { exact: true })).toBeVisible({
    timeout: 30_000,
  });
  await expect(page.getByText(/^Reconnecting…/)).toHaveCount(0);
});

test("keeps sign-in, Today, Work and the item usable at a phone viewport", async ({
  page,
}) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await connect(page);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(greeting);
  await expect
    .poll(() =>
      page.evaluate(
        () =>
          document.documentElement.scrollWidth <=
          document.documentElement.clientWidth + 1,
      ),
    )
    .toBe(true);

  await page
    .getByRole("navigation", { name: "Tabs" })
    .getByRole("link", { name: /^Work/ })
    .click();
  // The phone Work screen is a filtered list, not the desktop board.
  await expect(
    page.getByRole("heading", { level: 1, name: "All work" }),
  ).toBeVisible();
  // It opens on Needs you; the Done filter lists finished work.
  await page
    .getByRole("group", { name: "Work", exact: true })
    .getByRole("button", { name: /^Done/ })
    .click();
  await page.getByRole("link", { name: new RegExp(doneTitle) }).click();
  const detail = page.getByRole("dialog", { name: doneTitle });
  await expect(detail).toBeVisible();
  await expect(detail.getByRole("heading", { name: "Timeline" })).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(detail).toHaveCount(0);
});
