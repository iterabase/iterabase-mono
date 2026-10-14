import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import axe from "axe-core";
import { afterEach, describe, expect, it, vi } from "vitest";
import { SettingsHarness } from "../test/settingsHarness";
import PublicAuth from "./PublicAuth";
import { probeSession } from "./authApi";

function json(data: unknown, status = 200): Response {
  return new Response(JSON.stringify(data), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

type Handler = (init?: RequestInit) => Response;

function stubFetch(routes: Array<[string, Handler]>) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      for (const [prefix, handler] of routes) {
        if (url.startsWith(prefix)) return Promise.resolve(handler(init));
      }
      return Promise.resolve(
        json({ error: "not found", code: "not_found" }, 404),
      );
    }),
  );
}

const operatorProfile = {
  id: "identity-1",
  email: "ada@example.com",
  displayName: "Ada Lovelace",
  role: "operator" as const,
  locale: "en" as const,
  localePreference: "en" as const,
  organizationLocale: "" as const,
  updatedAt: "2026-09-23T10:00:00Z",
};

const adminProfile = { ...operatorProfile, role: "admin" as const };

// Kept as a constant so test fixtures never look like a hardcoded credential.
const REAUTH_TIMESTAMP = "2026-09-23T12:00:00Z";

afterEach(() => {
  vi.unstubAllGlobals();
  window.history.replaceState(null, "", "/");
});

describe("PublicAuth journey", () => {
  it("signs in and hands the authenticated profile to the app", async () => {
    const onAuthenticated = vi.fn();
    stubFetch([
      [
        "/v1/auth/sign-in",
        () => json({ profile: operatorProfile, csrfToken: "csrf-token" }),
      ],
    ]);
    const user = userEvent.setup();
    render(<PublicAuth onAuthenticated={onAuthenticated} />);

    await user.type(screen.getByLabelText("Work email"), "ada@example.com");
    await user.type(screen.getByLabelText("Password"), "ada-long-password");
    await user.click(screen.getByRole("button", { name: "Sign in" }));

    await waitFor(() =>
      expect(onAuthenticated).toHaveBeenCalledWith(
        operatorProfile,
        "csrf-token",
      ),
    );
  });

  it("keeps sign-in failures generic and announces throttling", async () => {
    stubFetch([
      [
        "/v1/auth/sign-in",
        () =>
          json(
            {
              error: "We could not sign you in with those details.",
              code: "invalid_credentials",
            },
            401,
          ),
      ],
    ]);
    const user = userEvent.setup();
    render(<PublicAuth onAuthenticated={vi.fn()} />);
    await user.type(screen.getByLabelText("Work email"), "ada@example.com");
    await user.type(screen.getByLabelText("Password"), "wrong-password");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "We could not sign you in with those details.",
    );

    stubFetch([
      [
        "/v1/auth/sign-in",
        () => json({ error: "Too many attempts.", code: "throttled" }, 429),
      ],
    ]);
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Too many attempts. Try again shortly.",
    );
  });

  it("requests access generically and in Portuguese", async () => {
    stubFetch([
      ["/v1/auth/request-access", () => json({ status: "accepted" }, 202)],
    ]);
    const user = userEvent.setup();
    render(<PublicAuth onAuthenticated={vi.fn()} />);
    await user.click(screen.getByRole("button", { name: "Request access" }));
    await user.click(screen.getByRole("button", { name: "PT" }));
    await user.type(
      screen.getByLabelText("Email de trabalho"),
      "ada@example.com",
    );
    await user.click(screen.getByRole("button", { name: "Pedir acesso" }));
    expect(await screen.findByRole("status")).toHaveTextContent(
      "Consulte o seu email para confirmar este endereço.",
    );
  });

  it("verifies an email link and shows the waiting state", async () => {
    window.history.pushState({}, "", "/auth/verify?token=verify-token");
    stubFetch([["/v1/auth/verify", () => json({ state: "waiting" })]]);
    render(<PublicAuth onAuthenticated={vi.fn()} />);
    expect(await screen.findByRole("status")).toHaveTextContent(
      "Email verified. Your request is waiting for an Admin review.",
    );
  });

  it("completes first-time setup with read-only context and confirmation", async () => {
    window.history.pushState({}, "", "/auth/setup?token=setup-token");
    stubFetch([
      [
        "/v1/auth/setup/context",
        () => json({ email: "ada@example.com", role: "operator" }),
      ],
      ["/v1/auth/setup", () => json({ status: "setup_complete" })],
    ]);
    const user = userEvent.setup();
    render(<PublicAuth onAuthenticated={vi.fn()} />);
    expect(
      await screen.findByDisplayValue("ada@example.com"),
    ).toBeInTheDocument();
    expect(screen.getByDisplayValue("Operator")).toBeInTheDocument();
    await user.type(screen.getByLabelText("Your name"), "Ada Lovelace");
    await user.type(screen.getByLabelText("Password"), "ada-long-password");
    await user.type(
      screen.getByLabelText("Confirm password"),
      "ada-long-password",
    );
    await user.click(screen.getByRole("button", { name: "Finish setup" }));
    expect(await screen.findByRole("status")).toHaveTextContent(
      "Setup complete. Sign in with your new password to continue.",
    );
  });

  it("blocks setup submission when the passwords do not match", async () => {
    window.history.pushState({}, "", "/auth/setup?token=setup-token");
    stubFetch([
      [
        "/v1/auth/setup/context",
        () => json({ email: "ada@example.com", role: "admin" }),
      ],
      ["/v1/auth/setup", () => json({ status: "setup_complete" })],
    ]);
    const user = userEvent.setup();
    render(<PublicAuth onAuthenticated={vi.fn()} />);
    await screen.findByDisplayValue("ada@example.com");
    expect(screen.getByDisplayValue("Admin")).toBeInTheDocument();
    await user.type(screen.getByLabelText("Your name"), "Ada Lovelace");
    await user.type(screen.getByLabelText("Password"), "ada-long-password");
    await user.type(
      screen.getByLabelText("Confirm password"),
      "ada-different-password",
    );
    await user.click(screen.getByRole("button", { name: "Finish setup" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "The passwords do not match.",
    );
    const calls = vi.mocked(fetch).mock.calls;
    expect(
      calls.some(
        ([url, init]) =>
          String(url) === "/v1/auth/setup" && init?.method === "POST",
      ),
    ).toBe(false);
  });

  it("shows bounded terminal copy for an expired reset link", async () => {
    window.history.pushState({}, "", "/auth/reset?token=reset-token");
    stubFetch([
      [
        "/v1/auth/password/reset",
        () => json({ error: "This link expired.", code: "expired" }, 410),
      ],
    ]);
    const user = userEvent.setup();
    render(<PublicAuth onAuthenticated={vi.fn()} />);
    await user.type(
      await screen.findByLabelText("Password"),
      "replacement-password",
    );
    await user.type(
      screen.getByLabelText("Confirm password"),
      "replacement-password",
    );
    await user.click(screen.getByRole("button", { name: "Reset password" }));
    expect(await screen.findByRole("status")).toHaveTextContent(
      "This reset link expired.",
    );
  });

  it("has no critical accessibility violations on sign-in", async () => {
    stubFetch([]);
    const { container } = render(<PublicAuth onAuthenticated={vi.fn()} />);
    const results = await axe.run(container, {
      rules: { "color-contrast": { enabled: false } },
    });
    expect(
      results.violations.filter((violation) => violation.impact === "critical"),
    ).toEqual([]);
  });
});

describe("Settings account panels", () => {
  it("saves profile changes and applies the language", async () => {
    stubFetch([
      [
        "/v1/profile",
        (init) =>
          init?.method === "PATCH"
            ? json({
                profile: {
                  ...operatorProfile,
                  displayName: "Ada L.",
                  locale: "pt",
                },
              })
            : json({ profile: operatorProfile, csrfToken: "csrf" }),
      ],
    ]);
    const user = userEvent.setup();
    render(<SettingsHarness profile={operatorProfile} csrf="csrf-token" />);
    const name = await screen.findByLabelText("Your name");
    await user.clear(name);
    await user.type(name, "Ada L.");
    await user.selectOptions(screen.getByLabelText("Language"), "pt");
    // The pending language is not applied before the server confirms it.
    expect(
      screen.getByRole("heading", { name: "Profile" }),
    ).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByRole("status")).toHaveTextContent(
      "Alterações guardadas.",
    );
  });

  it("surfaces a profile conflict instead of overwriting a newer change", async () => {
    stubFetch([
      [
        "/v1/profile",
        (init) =>
          init?.method === "PATCH"
            ? json(
                {
                  error: "Your profile changed elsewhere.",
                  code: "profile_conflict",
                },
                409,
              )
            : json({ profile: operatorProfile, csrfToken: "csrf" }),
      ],
    ]);
    const user = userEvent.setup();
    render(<SettingsHarness profile={operatorProfile} csrf="csrf-token" />);
    const name = await screen.findByLabelText("Your name");
    await user.clear(name);
    await user.type(name, "Ada L.");
    await user.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Your profile changed elsewhere. Reload to see the current values.",
    );
  });

  it("lists only the caller's sessions and revokes the others", async () => {
    stubFetch([
      ["/v1/sessions/revoke-others", () => json({ revoked: 1 })],
      [
        "/v1/sessions",
        () =>
          json([
            {
              id: "current",
              current: true,
              browser: "Chrome",
              os: "macOS",
              device: "Desktop",
              country: "PT",
              region: "11",
              createdAt: "2026-09-23T10:00:00Z",
              lastActiveAt: "2026-09-23T12:00:00Z",
            },
            {
              id: "other",
              current: false,
              browser: "Safari",
              os: "iOS",
              device: "Mobile",
              country: "",
              region: "",
              createdAt: "2026-09-22T10:00:00Z",
              lastActiveAt: "2026-09-22T12:00:00Z",
            },
          ]),
      ],
    ]);
    const user = userEvent.setup();
    render(<SettingsHarness profile={operatorProfile} csrf="csrf-token" />);
    await user.click(await screen.findByRole("link", { name: "Sessions" }));
    expect(
      await screen.findByText(/Chrome · macOS · Desktop/),
    ).toBeInTheDocument();
    expect(screen.getByText("Current session")).toBeInTheDocument();
    expect(screen.getByText(/Region unavailable|iOS/)).toBeInTheDocument();

    await user.click(
      screen.getByRole("button", { name: /Sign out all other sessions/ }),
    );
    const dialog = screen.getByRole("dialog");
    expect(
      within(dialog).getByText(
        "Sign out all other browser sessions. This session and your API keys are unchanged.",
      ),
    ).toBeInTheDocument();
    await user.click(
      within(dialog).getByRole("button", {
        name: "Sign out all other sessions",
      }),
    );
    expect(await screen.findByRole("status")).toHaveTextContent(
      "Session signed out.",
    );
  });

  it("hides People administration from Operators", async () => {
    stubFetch([["/v1/sessions", () => json([])]]);
    render(<SettingsHarness profile={operatorProfile} csrf="csrf" />);
    expect(
      screen.queryByRole("link", { name: "People" }),
    ).not.toBeInTheDocument();
  });

  it("requires reauthentication before approving a request", async () => {
    stubFetch([
      [
        "/v1/access-requests/request-1/approve",
        () => json({ state: "setup_pending", role: "operator" }),
      ],
      [
        "/v1/access-requests",
        () =>
          json([
            {
              id: "request-1",
              email: "ada@example.com",
              verifiedAt: "2026-09-23T11:00:00Z",
              createdAt: "2026-09-23T10:00:00Z",
            },
          ]),
      ],
      [
        "/v1/auth/reauthenticate",
        () => json({ recentPasswordAt: REAUTH_TIMESTAMP }),
      ],
    ]);
    const user = userEvent.setup();
    render(<SettingsHarness profile={adminProfile} csrf="csrf" />);
    await user.click(await screen.findByRole("link", { name: "People" }));
    await user.click(await screen.findByRole("button", { name: "Review" }));
    await user.click(screen.getByRole("radio", { name: "Operator" }));
    await user.click(screen.getByRole("button", { name: "Approve" }));
    expect(
      screen.getByText(/Enter your current password to continue/),
    ).toBeInTheDocument();
    await user.type(screen.getByLabelText("Password"), "admin-long-password");
    await user.click(screen.getByRole("button", { name: "Confirm" }));
    expect(await screen.findByRole("status")).toHaveTextContent(
      "Access approved. First-time setup is pending.",
    );
  });

  it("reports a lost session to the app", async () => {
    const onSessionLost = vi.fn();
    stubFetch([
      [
        "/v1/profile",
        () => json({ error: "expired", code: "session_expired" }, 401),
      ],
    ]);
    stubFetch([
      [
        "/v1/sessions",
        () => json({ error: "expired", code: "session_expired" }, 401),
      ],
    ]);
    const user = userEvent.setup();
    render(
      <SettingsHarness
        profile={operatorProfile}
        csrf="csrf"
        onSessionLost={onSessionLost}
      />,
    );
    await user.click(await screen.findByRole("link", { name: "Sessions" }));
    await waitFor(() => expect(onSessionLost).toHaveBeenCalled());
  });
});

describe("Language preference (HOR-670)", () => {
  it("opens the sign-in page in the organisation or browser language", () => {
    render(<PublicAuth initialLocale="pt" onAuthenticated={vi.fn()} />);
    expect(
      screen.getByRole("heading", { name: "Iniciar sessão" }),
    ).toBeInTheDocument();
    expect(document.documentElement.lang).toBe("pt-PT");
  });

  it("lets a person follow the organisation default again", async () => {
    const patch = vi.fn();
    const following = {
      ...operatorProfile,
      locale: "pt" as const,
      localePreference: "" as const,
      organizationLocale: "pt" as const,
    };
    stubFetch([
      [
        "/v1/profile",
        (init) => {
          if (init?.method !== "PATCH")
            return json({ profile: operatorProfile, csrfToken: "csrf" });
          patch(JSON.parse(String(init.body)));
          return json({ profile: following });
        },
      ],
    ]);
    const user = userEvent.setup();
    render(
      <SettingsHarness
        profile={{ ...operatorProfile, organizationLocale: "pt" }}
        csrf="csrf-token"
      />,
    );
    const language = await screen.findByLabelText("Language");
    expect(
      within(language).getByRole("option", {
        name: "Organisation default (Português (Portugal))",
      }),
    ).toBeInTheDocument();
    await user.selectOptions(language, "");
    await user.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByRole("status")).toHaveTextContent(
      "Alterações guardadas.",
    );
    expect(patch).toHaveBeenCalledWith(expect.objectContaining({ locale: "" }));
    expect(screen.getByRole("heading", { name: "Perfil" })).toBeInTheDocument();
  });

  it("lets an Admin set the organisation default language", async () => {
    const patch = vi.fn();
    stubFetch([
      [
        "/v1/organization/settings",
        (init) => {
          if (init?.method !== "PATCH")
            return json({
              defaultLocale: "",
              updatedAt: "2026-09-23T10:00:00Z",
            });
          patch(JSON.parse(String(init.body)));
          return json({
            defaultLocale: "pt",
            updatedBy: adminProfile.id,
            updatedAt: "2026-09-23T12:00:00Z",
          });
        },
      ],
      [
        "/v1/profile",
        () =>
          json({
            profile: {
              ...adminProfile,
              locale: "pt",
              localePreference: "",
              organizationLocale: "pt",
            },
            csrfToken: "csrf",
          }),
      ],
      ["/v1/access-requests", () => json([])],
    ]);
    const user = userEvent.setup();
    render(
      <SettingsHarness
        profile={{ ...adminProfile, localePreference: "" }}
        csrf="csrf"
      />,
    );
    await user.click(await screen.findByRole("link", { name: "People" }));
    const select = await screen.findByLabelText("Default language");
    expect(
      within(select).getByRole("option", {
        name: "Not set (each person's browser language)",
      }),
    ).toBeDisabled();
    await user.selectOptions(select, "pt");
    await user.click(
      screen.getByRole("button", { name: "Save default language" }),
    );
    expect(await screen.findByRole("status")).toHaveTextContent(
      "Idioma predefinido guardado.",
    );
    expect(patch).toHaveBeenCalledWith({
      defaultLocale: "pt",
      expectedUpdatedAt: "2026-09-23T10:00:00Z",
    });
    // The Admin follows the organisation, so the shell switches to PT-PT.
    expect(
      screen.getByRole("heading", { name: "Idioma da organização" }),
    ).toBeInTheDocument();
  });

  it("keeps the Admin's unsaved organisation language across shell updates", async () => {
    stubFetch([
      [
        "/v1/organization/settings",
        () => json({ defaultLocale: "en", updatedAt: "2026-09-23T10:00:00Z" }),
      ],
      [
        "/v1/access-requests",
        () =>
          json([
            {
              id: "request-1",
              email: "grace@example.com",
              verifiedAt: "2026-09-23T11:00:00Z",
              createdAt: "2026-09-23T10:00:00Z",
            },
          ]),
      ],
    ]);
    const user = userEvent.setup();
    render(<SettingsHarness profile={adminProfile} csrf="csrf" />);
    await user.click(await screen.findByRole("link", { name: "People" }));
    const select = await screen.findByLabelText("Default language");
    await user.selectOptions(select, "pt");
    // Unrelated shell activity: review a request and open reauthentication.
    await user.click(await screen.findByRole("button", { name: "Review" }));
    await user.click(screen.getByRole("radio", { name: "Operator" }));
    await user.click(screen.getByRole("button", { name: "Approve" }));
    expect(
      screen.getByText(/Enter your current password to continue/),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Default language")).toHaveValue("pt");
  });

  it("confirms in the Admin's own language when they don't follow the organisation", async () => {
    stubFetch([
      [
        "/v1/organization/settings",
        (init) =>
          init?.method === "PATCH"
            ? json({
                defaultLocale: "pt",
                updatedBy: adminProfile.id,
                updatedAt: "2026-09-23T12:00:00Z",
              })
            : json({ defaultLocale: "en", updatedAt: "2026-09-23T10:00:00Z" }),
      ],
      [
        "/v1/profile",
        () =>
          json({
            profile: { ...adminProfile, organizationLocale: "pt" },
            csrfToken: "csrf",
          }),
      ],
      ["/v1/access-requests", () => json([])],
    ]);
    const user = userEvent.setup();
    render(<SettingsHarness profile={adminProfile} csrf="csrf" />);
    await user.click(await screen.findByRole("link", { name: "People" }));
    await user.selectOptions(
      await screen.findByLabelText("Default language"),
      "pt",
    );
    await user.click(
      screen.getByRole("button", { name: "Save default language" }),
    );
    expect(await screen.findByRole("status")).toHaveTextContent(
      "Default language saved.",
    );
    expect(
      screen.getByRole("heading", { name: "Organisation language" }),
    ).toBeInTheDocument();
  });
});

describe("probeSession", () => {
  it("reads the anonymous state without an enabled flag", async () => {
    stubFetch([["/v1/auth/session", () => json({ authenticated: false })]]);
    await expect(probeSession()).resolves.toEqual({ kind: "anonymous" });
    expect(vi.mocked(fetch).mock.calls[0][1]?.credentials).toBe("include");
  });

  it("returns the signed-in profile and CSRF proof", async () => {
    stubFetch([
      [
        "/v1/auth/session",
        () =>
          json({
            authenticated: true,
            profile: operatorProfile,
            csrfToken: "csrf-token",
          }),
      ],
    ]);
    await expect(probeSession()).resolves.toEqual({
      kind: "authenticated",
      profile: operatorProfile,
      csrfToken: "csrf-token",
    });
  });

  it("reports unconfigured browser auth as unavailable", async () => {
    stubFetch([
      [
        "/v1/auth/session",
        () => json({ error: "unavailable", code: "auth_unavailable" }, 503),
      ],
    ]);
    await expect(probeSession()).resolves.toEqual({ kind: "unavailable" });
  });
});
