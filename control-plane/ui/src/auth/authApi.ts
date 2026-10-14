export type Role = "admin" | "operator";
export type LocaleCode = "en" | "pt";

export interface AuthProfile {
  id: string;
  email: string;
  displayName: string;
  role: Role;
  /** The effective language: own choice, organisation, browser, English. */
  locale: LocaleCode;
  /** The person's own choice; "" follows the organisation default. */
  localePreference: LocaleCode | "";
  /** The organisation default; "" until an Admin chooses one. */
  organizationLocale: LocaleCode | "";
  updatedAt: string;
}

export interface OrganizationSettings {
  defaultLocale: LocaleCode | "";
  /** The installation's name (DES-HOR-459-08); "" until set. */
  name?: string;
  updatedBy?: string;
  updatedAt: string;
}

export interface SessionInfo {
  id: string;
  current: boolean;
  browser?: string;
  os?: string;
  device?: string;
  country?: string;
  region?: string;
  createdAt: string;
  lastActiveAt: string;
}

export interface AccessRequest {
  id: string;
  email: string;
  verifiedAt?: string;
  createdAt: string;
}

/** Bounded customer-safe error contract returned by the auth endpoints. */
export class AuthAPIError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
  ) {
    super(message);
    this.name = "AuthAPIError";
  }
}

async function request<T>(
  path: string,
  init: RequestInit = {},
  csrf?: string,
): Promise<T> {
  const headers: Record<string, string> = {
    ...(init.body ? { "Content-Type": "application/json" } : {}),
    ...(csrf ? { "X-CSRF-Token": csrf } : {}),
  };
  const response = await fetch(path, {
    ...init,
    credentials: "include",
    headers: { ...headers, ...(init.headers as Record<string, string>) },
  });
  if (!response.ok) {
    let code = "error";
    let message = response.statusText || "Request failed";
    try {
      const payload = (await response.json()) as {
        error?: string;
        code?: string;
      };
      code = payload.code || code;
      message = payload.error || message;
    } catch {
      /* empty body */
    }
    throw new AuthAPIError(response.status, code, message);
  }
  if (response.status === 204) return undefined as T;
  return (await response.json()) as T;
}

export const authApi = {
  profile: () =>
    request<{ profile: AuthProfile; csrfToken: string }>("/v1/profile"),
  requestAccess: (email: string, locale: LocaleCode) =>
    request<{ status: string }>("/v1/auth/request-access", {
      method: "POST",
      body: JSON.stringify({ email, locale }),
    }),
  verify: (token: string) =>
    request<{ state: string }>("/v1/auth/verify", {
      method: "POST",
      body: JSON.stringify({ token }),
    }),
  requestStatus: (token: string) =>
    request<{ state: string }>("/v1/auth/request-status", {
      method: "POST",
      body: JSON.stringify({ token }),
    }),
  setup: (
    token: string,
    displayName: string,
    locale: LocaleCode,
    password: string,
  ) =>
    request<{ status: string }>("/v1/auth/setup", {
      method: "POST",
      body: JSON.stringify({ token, displayName, locale, password }),
    }),
  setupContext: (token: string) =>
    request<{ email: string; role: Role }>("/v1/auth/setup/context", {
      method: "POST",
      body: JSON.stringify({ token }),
    }),
  resendSetup: (token: string) =>
    request<{ status: string }>("/v1/auth/setup/resend", {
      method: "POST",
      body: JSON.stringify({ token }),
    }),
  signIn: (email: string, password: string) =>
    request<{ profile: AuthProfile; csrfToken: string }>("/v1/auth/sign-in", {
      method: "POST",
      body: JSON.stringify({ email, password }),
    }),
  signOut: (csrf: string) =>
    request<void>("/v1/auth/sign-out", { method: "POST" }, csrf),
  /**
   * Terminates this browser session. A stale CSRF proof (another tab or
   * surface rotated it) is refreshed once and retried; a session that is
   * already gone counts as signed out. Any other failure is thrown so the
   * caller never presents a still-valid session as signed out.
   */
  signOutCurrent: async (csrf: string): Promise<void> => {
    try {
      await authApi.signOut(csrf);
    } catch (err) {
      if (!(err instanceof AuthAPIError)) throw err;
      if (err.status === 401) return;
      if (err.code !== "csrf") throw err;
      let refreshed: { csrfToken: string };
      try {
        refreshed = await authApi.profile();
      } catch (refreshErr) {
        if (refreshErr instanceof AuthAPIError && refreshErr.status === 401)
          return;
        throw refreshErr;
      }
      await authApi.signOut(refreshed.csrfToken);
    }
  },
  forgot: (email: string) =>
    request<{ status: string }>("/v1/auth/password/forgot", {
      method: "POST",
      body: JSON.stringify({ email }),
    }),
  reset: (token: string, password: string) =>
    request<{ status: string }>("/v1/auth/password/reset", {
      method: "POST",
      body: JSON.stringify({ token, password }),
    }),
  updateProfile: (
    csrf: string,
    body: {
      displayName?: string;
      locale?: LocaleCode | "";
      expectedUpdatedAt?: string;
    },
  ) =>
    request<{ profile: AuthProfile }>(
      "/v1/profile",
      { method: "PATCH", body: JSON.stringify(body) },
      csrf,
    ),
  reauthenticate: (csrf: string, password: string) =>
    request<{ recentPasswordAt: string }>(
      "/v1/auth/reauthenticate",
      { method: "POST", body: JSON.stringify({ password }) },
      csrf,
    ),
  sessions: () => request<SessionInfo[]>("/v1/sessions"),
  revokeSession: (csrf: string, id: string) =>
    request<void>(`/v1/sessions/${id}`, { method: "DELETE" }, csrf),
  revokeOtherSessions: (csrf: string) =>
    request<{ revoked: number }>(
      "/v1/sessions/revoke-others",
      { method: "POST" },
      csrf,
    ),
  accessRequests: () => request<AccessRequest[]>("/v1/access-requests"),
  organizationSettings: () =>
    request<OrganizationSettings>("/v1/organization/settings"),
  updateOrganizationSettings: (
    csrf: string,
    body: {
      defaultLocale?: LocaleCode;
      name?: string;
      expectedUpdatedAt?: string;
    },
  ) =>
    request<OrganizationSettings>(
      "/v1/organization/settings",
      { method: "PATCH", body: JSON.stringify(body) },
      csrf,
    ),
  approve: (csrf: string, id: string, role: Role) =>
    request<{ state: string; role: Role }>(
      `/v1/access-requests/${id}/approve`,
      { method: "POST", body: JSON.stringify({ role }) },
      csrf,
    ),
  decline: (csrf: string, id: string) =>
    request<{ state: string }>(
      `/v1/access-requests/${id}/decline`,
      { method: "POST" },
      csrf,
    ),
};

export type ProbeResult =
  | {
      kind: "authenticated";
      profile: AuthProfile;
      csrfToken: string;
      organizationName: string;
    }
  | { kind: "anonymous"; locale?: LocaleCode; organizationName: string }
  | { kind: "unavailable" };

/**
 * Probe the browser-session bootstrap endpoint. Sign-in is always on: it
 * answers 200 for both the anonymous and signed-in states, and 503 only when
 * browser auth is unavailable.
 */
export async function probeSession(): Promise<ProbeResult> {
  try {
    const response = await fetch("/v1/auth/session", {
      credentials: "include",
      headers: { Accept: "application/json" },
    });
    if (!response.ok) return { kind: "unavailable" };
    const payload = (await response.json()) as {
      authenticated?: boolean;
      profile?: AuthProfile;
      csrfToken?: string;
      locale?: string;
      organizationName?: string;
    };
    const organizationName = payload.organizationName?.trim() ?? "";
    if (payload.authenticated && payload.profile && payload.csrfToken) {
      return {
        kind: "authenticated",
        profile: payload.profile,
        csrfToken: payload.csrfToken,
        organizationName,
      };
    }
    return {
      kind: "anonymous",
      organizationName,
      locale:
        payload.locale === "pt" || payload.locale === "en"
          ? payload.locale
          : undefined,
    };
  } catch {
    return { kind: "unavailable" };
  }
}
