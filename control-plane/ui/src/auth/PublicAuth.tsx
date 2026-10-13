import {
  FormEvent,
  RefObject,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { AuthAPIError, AuthProfile, LocaleCode, authApi } from "./authApi";
import type { TFunction } from "i18next";
import { fixedT, setLocale as setActiveLocale } from "../i18n";

type AuthT = TFunction<"auth">;

type View =
  | { kind: "sign-in" }
  | { kind: "request" }
  | { kind: "request-accepted" }
  | { kind: "forgot" }
  | { kind: "forgot-accepted" }
  | { kind: "verify-checking"; token: string }
  | { kind: "request-state"; state: string; token: string }
  | { kind: "setup"; token: string }
  | {
      kind: "setup-terminal";
      reason: "expired" | "reused" | "superseded" | "ineligible";
      token: string;
    }
  | { kind: "setup-done" }
  | { kind: "reset"; token: string }
  | {
      kind: "reset-terminal";
      reason: "expired" | "reused" | "superseded" | "ineligible";
    }
  | { kind: "reset-done" }
  | {
      kind: "verify-error";
      reason: "invalid" | "expired" | "superseded" | "unavailable";
    };

interface Props {
  initialMessage?: string;
  /** The organisation default, else the browser language (DES-HOR-670-02). */
  initialLocale?: LocaleCode;
  onAuthenticated: (profile: AuthProfile, csrfToken: string) => void;
}

function initialState(): View {
  if (typeof window === "undefined") return { kind: "sign-in" };
  const params = new URLSearchParams(window.location.search);
  const token = params.get("token") || "";
  switch (window.location.pathname) {
    case "/auth/verify":
      return token
        ? { kind: "verify-checking", token }
        : { kind: "verify-error", reason: "invalid" };
    case "/auth/setup":
      return token
        ? { kind: "setup", token }
        : { kind: "setup-terminal", reason: "reused", token: "" };
    case "/auth/reset":
      return token
        ? { kind: "reset", token }
        : { kind: "reset-terminal", reason: "reused" };
    default:
      return { kind: "sign-in" };
  }
}

export default function PublicAuth({
  initialMessage,
  initialLocale = "en",
  onAuthenticated,
}: Props) {
  const [locale, setLocale] = useState<LocaleCode>(initialLocale);
  const [view, setView] = useState<View>(initialState);
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [passwordMismatch, setPasswordMismatch] = useState("");
  const [setupContext, setSetupContext] = useState<{
    email: string;
    role: string;
  } | null>(null);
  const [contextLoading, setContextLoading] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(initialMessage || "");
  const [notice, setNotice] = useState("");
  const headingRef = useRef<HTMLHeadingElement>(null);
  const c = useMemo(() => fixedT(locale, "auth"), [locale]);

  useEffect(() => {
    window.history.replaceState(null, "", window.location.pathname);
  }, []);

  useEffect(() => setActiveLocale(locale), [locale]);

  // A valid setup link discloses only its bounded read-only context: the
  // verified work email and the approved role.
  useEffect(() => {
    if (view.kind !== "setup") return;
    let cancelled = false;
    setContextLoading(true);
    void (async () => {
      try {
        const context = await authApi.setupContext(view.token);
        if (!cancelled) setSetupContext(context);
      } catch (err) {
        if (cancelled) return;
        const code = err instanceof AuthAPIError ? err.code : "unavailable";
        if (
          code === "expired" ||
          code === "reused" ||
          code === "superseded" ||
          code === "ineligible" ||
          code === "invalid"
        ) {
          setView({
            kind: "setup-terminal",
            reason: code === "invalid" ? "reused" : code,
            token: view.token,
          });
        }
      } finally {
        if (!cancelled) setContextLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [view]);

  // Verify links are consumed on load; the waiting state polls the same
  // browser-held secret for the Admin outcome.
  useEffect(() => {
    if (view.kind !== "verify-checking") return;
    let cancelled = false;
    void (async () => {
      try {
        const result = await authApi.verify(view.token);
        if (cancelled) return;
        sessionStorage.setItem("iterabase.requestToken", view.token);
        setView({
          kind: "request-state",
          state: result.state,
          token: view.token,
        });
      } catch (err) {
        if (cancelled) return;
        const code = err instanceof AuthAPIError ? err.code : "unavailable";
        setView({ kind: "verify-error", reason: reasonFor(code) });
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [view]);

  useEffect(() => {
    if (view.kind !== "request-state" || view.state !== "waiting") return;
    const token = view.token;
    const timer = window.setInterval(() => {
      void (async () => {
        try {
          const result = await authApi.requestStatus(token);
          setView((current) =>
            current.kind === "request-state" && current.state !== result.state
              ? { ...current, state: result.state }
              : current,
          );
        } catch {
          /* a transient poll failure keeps the last known state */
        }
      })();
    }, 10_000);
    return () => window.clearInterval(timer);
  }, [view]);

  async function run(action: () => Promise<void>) {
    setError("");
    setNotice("");
    setBusy(true);
    try {
      await action();
    } catch (err) {
      const code = err instanceof AuthAPIError ? err.code : "unavailable";
      setError(messageFor(code, c));
    } finally {
      setBusy(false);
    }
  }

  function onSignIn(event: FormEvent) {
    event.preventDefault();
    void run(async () => {
      const result = await authApi.signIn(email, password);
      onAuthenticated(result.profile, result.csrfToken);
    });
  }

  function onRequestAccess(event: FormEvent) {
    event.preventDefault();
    void run(async () => {
      await authApi.requestAccess(email, locale);
      setView({ kind: "request-accepted" });
    });
  }

  function onForgot(event: FormEvent) {
    event.preventDefault();
    void run(async () => {
      await authApi.forgot(email);
      setView({ kind: "forgot-accepted" });
    });
  }

  function onSetup(event: FormEvent) {
    event.preventDefault();
    if (view.kind !== "setup") return;
    if (password !== confirmPassword) {
      setPasswordMismatch(c("passwordMismatch"));
      return;
    }
    setPasswordMismatch("");
    const token = view.token;
    void run(async () => {
      try {
        await authApi.setup(token, displayName, locale, password);
        setView({ kind: "setup-done" });
      } catch (err) {
        const code = err instanceof AuthAPIError ? err.code : "unavailable";
        if (
          code === "expired" ||
          code === "reused" ||
          code === "superseded" ||
          code === "ineligible"
        ) {
          setView({ kind: "setup-terminal", reason: code, token });
          return;
        }
        throw err;
      }
    });
  }

  function onReset(event: FormEvent) {
    event.preventDefault();
    if (view.kind !== "reset") return;
    if (password !== confirmPassword) {
      setPasswordMismatch(c("passwordMismatch"));
      return;
    }
    setPasswordMismatch("");
    const token = view.token;
    void run(async () => {
      try {
        await authApi.reset(token, password);
        setView({ kind: "reset-done" });
      } catch (err) {
        const code = err instanceof AuthAPIError ? err.code : "unavailable";
        if (
          code === "expired" ||
          code === "reused" ||
          code === "superseded" ||
          code === "ineligible"
        ) {
          setView({ kind: "reset-terminal", reason: code });
          return;
        }
        throw err;
      }
    });
  }

  function onResendSetup() {
    if (view.kind !== "setup-terminal") return;
    void run(async () => {
      await authApi.resendSetup(view.token);
      setNotice(c("setupResendSent"));
    });
  }

  return (
    <main className="auth-page">
      <section className="auth-card" aria-live="polite">
        <header className="auth-brand">
          <span className="eyebrow">{c("eyebrow")}</span>
          <h1>{c("brand")}</h1>
          <div className="auth-locale" role="group" aria-label={c("language")}>
            <button
              type="button"
              className={locale === "en" ? "active" : ""}
              aria-pressed={locale === "en"}
              onClick={() => setLocale("en")}
            >
              {c("languageShort.en")}
            </button>
            <button
              type="button"
              className={locale === "pt" ? "active" : ""}
              aria-pressed={locale === "pt"}
              onClick={() => setLocale("pt")}
            >
              {c("languageShort.pt")}
            </button>
          </div>
        </header>

        {error && (
          <div className="form-error" role="alert">
            {error}
          </div>
        )}
        {notice && (
          <div className="auth-notice" role="status">
            {notice}
          </div>
        )}

        {(view.kind === "sign-in" ||
          view.kind === "request" ||
          view.kind === "request-accepted" ||
          view.kind === "forgot" ||
          view.kind === "forgot-accepted") && (
          <SignInView
            view={view}
            copy={c}
            busy={busy}
            email={email}
            password={password}
            locale={locale}
            setLocale={setLocale}
            setEmail={setEmail}
            setPassword={setPassword}
            onSignIn={onSignIn}
            onRequest={onRequestAccess}
            onForgot={onForgot}
            headingRef={headingRef}
          />
        )}

        {view.kind === "verify-checking" && (
          <div className="auth-state" aria-busy="true">
            <h2 ref={headingRef}>{c("verifyChecking")}</h2>
            <p className="loading">{c("loading")}</p>
          </div>
        )}

        {view.kind === "verify-error" && (
          <div className="auth-state" role="status">
            <h2>{verifyErrorTitle(view.reason, c)}</h2>
            <p>
              {view.reason === "expired" || view.reason === "superseded"
                ? c("verifyExpired")
                : c("verifyInvalid")}
            </p>
            <div className="auth-links">
              <button
                type="button"
                className="link-button"
                onClick={() => setView({ kind: "request" })}
              >
                {c("requestAccessLink")}
              </button>
              <button
                type="button"
                className="link-button"
                onClick={() => setView({ kind: "sign-in" })}
              >
                {c("backToSignIn")}
              </button>
            </div>
          </div>
        )}

        {view.kind === "request-state" && (
          <div className="auth-state" role="status">
            <h2>{requestStateTitle(view.state, c)}</h2>
            <p>{requestStateBody(view.state, c)}</p>
            <button type="button" onClick={() => setView({ kind: "sign-in" })}>
              {c("backToSignIn")}
            </button>
          </div>
        )}

        {view.kind === "setup" && (
          <form className="auth-form" onSubmit={onSetup} aria-busy={busy}>
            <h2>{c("setupTitle")}</h2>
            <p className="auth-intro">{c("setupIntro")}</p>
            {contextLoading && (
              <p className="loading" role="status">
                {c("loading")}
              </p>
            )}
            {setupContext && (
              <>
                <label>
                  <span>{c("email")}</span>
                  <input value={setupContext.email} readOnly disabled />
                </label>
                <label>
                  <span>{c("role")}</span>
                  <input
                    value={
                      setupContext.role === "admin"
                        ? c("roleAdmin")
                        : c("roleOperator")
                    }
                    readOnly
                    disabled
                  />
                </label>
              </>
            )}
            <label>
              <span>{c("displayName")}</span>
              <input
                required
                value={displayName}
                onChange={(event) => setDisplayName(event.target.value)}
              />
            </label>
            <label>
              <span>{c("language")}</span>
              <select
                value={locale}
                onChange={(event) =>
                  setLocale(event.target.value as LocaleCode)
                }
              >
                <option value="en">{c("languageName.en")}</option>
                <option value="pt">{c("languageName.pt")}</option>
              </select>
            </label>
            <PasswordFields
              copy={c}
              password={password}
              confirm={confirmPassword}
              setPassword={(value) => {
                setPassword(value);
                setPasswordMismatch("");
              }}
              setConfirm={(value) => {
                setConfirmPassword(value);
                setPasswordMismatch("");
              }}
              mismatch={passwordMismatch}
            />
            <button type="submit" disabled={busy}>
              {busy ? c("setupCompleting") : c("finishSetup")}
            </button>
          </form>
        )}

        {view.kind === "setup-terminal" && (
          <div className="auth-state" role="status">
            <h2>{setupTitle(view.reason, c)}</h2>
            <p>{setupBody(view.reason, c)}</p>
            <button type="button" disabled={busy} onClick={onResendSetup}>
              {c("setupResend")}
            </button>
            <button type="button" onClick={() => setView({ kind: "sign-in" })}>
              {c("backToSignIn")}
            </button>
          </div>
        )}

        {view.kind === "setup-done" && (
          <div className="auth-state" role="status">
            <h2>{c("setupDoneTitle")}</h2>
            <p>{c("setupDoneBody")}</p>
            <button type="button" onClick={() => setView({ kind: "sign-in" })}>
              {c("signIn")}
            </button>
          </div>
        )}

        {view.kind === "reset" && (
          <form className="auth-form" onSubmit={onReset} aria-busy={busy}>
            <h2>{c("resetTitle")}</h2>
            <p className="auth-intro">{c("resetIntro")}</p>
            <PasswordFields
              copy={c}
              password={password}
              confirm={confirmPassword}
              setPassword={(value) => {
                setPassword(value);
                setPasswordMismatch("");
              }}
              setConfirm={(value) => {
                setConfirmPassword(value);
                setPasswordMismatch("");
              }}
              mismatch={passwordMismatch}
            />
            <button type="submit" disabled={busy}>
              {busy ? c("resetCompleting") : c("resetSubmit")}
            </button>
          </form>
        )}

        {view.kind === "reset-terminal" && (
          <div className="auth-state" role="status">
            <h2>{resetTitle(view.reason, c)}</h2>
            <p>{resetBody(view.reason, c)}</p>
            <button type="button" onClick={() => setView({ kind: "sign-in" })}>
              {c("backToSignIn")}
            </button>
          </div>
        )}

        {view.kind === "reset-done" && (
          <div className="auth-state" role="status">
            <h2>{c("resetDoneTitle")}</h2>
            <p>{c("resetDoneBody")}</p>
            <button type="button" onClick={() => setView({ kind: "sign-in" })}>
              {c("signIn")}
            </button>
          </div>
        )}
      </section>
    </main>
  );
}

function PasswordFields({
  copy,
  password,
  confirm,
  setPassword,
  setConfirm,
  mismatch,
}: {
  copy: AuthT;
  password: string;
  confirm: string;
  setPassword: (value: string) => void;
  setConfirm: (value: string) => void;
  mismatch: string;
}) {
  return (
    <>
      <label>
        <span>{copy("password")}</span>
        <input
          type="password"
          autoComplete="new-password"
          minLength={12}
          maxLength={128}
          required
          value={password}
          onChange={(event) => setPassword(event.target.value)}
          aria-describedby="password-guidance"
        />
      </label>
      <label>
        <span>{copy("confirmPassword")}</span>
        <input
          type="password"
          autoComplete="new-password"
          minLength={12}
          maxLength={128}
          required
          value={confirm}
          onChange={(event) => setConfirm(event.target.value)}
          aria-describedby="password-guidance"
          aria-invalid={mismatch ? true : undefined}
        />
      </label>
      {mismatch && (
        <p className="form-error" role="alert">
          {mismatch}
        </p>
      )}
      <p id="password-guidance" className="auth-guidance">
        {copy("passwordGuidance")}
      </p>
    </>
  );
}

interface SignInViewProps {
  view: View;
  copy: AuthT;
  busy: boolean;
  email: string;
  password: string;
  locale: LocaleCode;
  setLocale: (value: LocaleCode) => void;
  setEmail: (value: string) => void;
  setPassword: (value: string) => void;
  onSignIn: (event: FormEvent) => void;
  onRequest: (event: FormEvent) => void;
  onForgot: (event: FormEvent) => void;
  headingRef: RefObject<HTMLHeadingElement | null>;
}

function SignInView({
  view,
  copy,
  busy,
  email,
  password,
  locale,
  setLocale,
  setEmail,
  setPassword,
  onSignIn,
  onRequest,
  onForgot,
  headingRef,
}: SignInViewProps) {
  const [mode, setMode] = useState<"sign-in" | "request" | "forgot">(
    view.kind === "request" || view.kind === "request-accepted"
      ? "request"
      : view.kind === "forgot" || view.kind === "forgot-accepted"
        ? "forgot"
        : "sign-in",
  );

  useEffect(() => {
    headingRef.current?.focus();
  }, [headingRef, mode]);

  if (view.kind === "request-accepted") {
    return (
      <div className="auth-state" role="status">
        <h2>{copy("requestTitle")}</h2>
        <p>{copy("requestAccepted")}</p>
        <button type="button" onClick={() => setMode("sign-in")}>
          {copy("backToSignIn")}
        </button>
      </div>
    );
  }
  if (view.kind === "forgot-accepted") {
    return (
      <div className="auth-state" role="status">
        <h2>{copy("forgotTitle")}</h2>
        <p>{copy("forgotAccepted")}</p>
        <button type="button" onClick={() => setMode("sign-in")}>
          {copy("backToSignIn")}
        </button>
      </div>
    );
  }

  if (mode === "request") {
    return (
      <form className="auth-form" onSubmit={onRequest} aria-busy={busy}>
        <h2 ref={headingRef} tabIndex={-1}>
          {copy("requestTitle")}
        </h2>
        <p className="auth-intro">{copy("requestIntro")}</p>
        <label>
          <span>{copy("email")}</span>
          <input
            type="email"
            autoComplete="email"
            required
            value={email}
            onChange={(event) => setEmail(event.target.value)}
          />
        </label>
        <label>
          <span>{copy("language")}</span>
          <select
            aria-label={copy("language")}
            value={locale}
            onChange={(event) => setLocale(event.target.value as LocaleCode)}
          >
            <option value="en">{copy("languageName.en")}</option>
            <option value="pt">{copy("languageName.pt")}</option>
          </select>
        </label>
        <button type="submit" disabled={busy}>
          {busy ? copy("requestSubmitting") : copy("requestSubmit")}
        </button>
        <button
          type="button"
          className="link-button"
          onClick={() => setMode("sign-in")}
        >
          {copy("backToSignIn")}
        </button>
      </form>
    );
  }

  if (mode === "forgot") {
    return (
      <form className="auth-form" onSubmit={onForgot} aria-busy={busy}>
        <h2 ref={headingRef} tabIndex={-1}>
          {copy("forgotTitle")}
        </h2>
        <p className="auth-intro">{copy("forgotIntro")}</p>
        <label>
          <span>{copy("email")}</span>
          <input
            type="email"
            autoComplete="email"
            required
            value={email}
            onChange={(event) => setEmail(event.target.value)}
          />
        </label>
        <button type="submit" disabled={busy}>
          {copy("forgotSubmit")}
        </button>
        <button
          type="button"
          className="link-button"
          onClick={() => setMode("sign-in")}
        >
          {copy("backToSignIn")}
        </button>
      </form>
    );
  }

  return (
    <form className="auth-form" onSubmit={onSignIn} aria-busy={busy}>
      <h2 ref={headingRef} tabIndex={-1}>
        {copy("signInTitle")}
      </h2>
      <p className="auth-intro">{copy("signInIntro")}</p>
      <label>
        <span>{copy("email")}</span>
        <input
          type="email"
          autoComplete="username"
          required
          value={email}
          onChange={(event) => setEmail(event.target.value)}
        />
      </label>
      <label>
        <span>{copy("password")}</span>
        <input
          type="password"
          autoComplete="current-password"
          required
          value={password}
          onChange={(event) => setPassword(event.target.value)}
        />
      </label>
      <button type="submit" disabled={busy}>
        {busy ? copy("signingIn") : copy("signIn")}
      </button>
      <div className="auth-links">
        <button
          type="button"
          className="link-button"
          onClick={() => setMode("forgot")}
        >
          {copy("forgotPassword")}
        </button>
        <button
          type="button"
          className="link-button"
          onClick={() => setMode("request")}
        >
          {copy("requestAccessLink")}
        </button>
      </div>
    </form>
  );
}

function reasonFor(
  code: string,
): "invalid" | "expired" | "superseded" | "unavailable" {
  switch (code) {
    case "expired":
      return "expired";
    case "superseded":
      return "superseded";
    case "invalid":
      return "invalid";
    default:
      return "unavailable";
  }
}

function verifyErrorTitle(reason: string, copy: AuthT): string {
  switch (reason) {
    case "expired":
      return copy("verifyExpired");
    case "superseded":
      return copy("verifySuperseded");
    case "unavailable":
      return copy("verifyUnavailable");
    default:
      return copy("verifyInvalid");
  }
}

function requestStateTitle(state: string, copy: AuthT): string {
  switch (state) {
    case "approved":
      return copy("verifyApproved");
    case "declined":
      return copy("verifyDeclined");
    case "expired":
      return copy("verifyExpired");
    default:
      return copy("verifyWaitingTitle");
  }
}

function requestStateBody(state: string, copy: AuthT): string {
  switch (state) {
    case "approved":
      return copy("verifyApproved");
    case "declined":
      return copy("verifyDeclined");
    case "expired":
      return copy("verifyExpired");
    default:
      return copy("verifyWaitingBody");
  }
}

function setupTitle(reason: string, copy: AuthT): string {
  switch (reason) {
    case "expired":
      return copy("setupExpired");
    case "superseded":
      return copy("setupSuperseded");
    case "ineligible":
      return copy("setupIneligible");
    default:
      return copy("setupReused");
  }
}

function setupBody(reason: string, copy: AuthT): string {
  return reason === "ineligible"
    ? copy("setupIneligible")
    : copy("setupRecover");
}

function resetTitle(reason: string, copy: AuthT): string {
  switch (reason) {
    case "expired":
      return copy("resetExpired");
    case "superseded":
      return copy("resetSuperseded");
    case "ineligible":
      return copy("resetIneligible");
    default:
      return copy("resetReused");
  }
}

function resetBody(reason: string, copy: AuthT): string {
  return reason === "ineligible"
    ? copy("resetIneligible")
    : copy("resetRecover");
}

function messageFor(code: string, copy: AuthT): string {
  switch (code) {
    case "throttled":
      return copy("throttled");
    case "invalid_email":
      return copy("invalidEmail");
    case "password_policy":
      return copy("passwordGuidance");
    case "session_expired":
      return copy("sessionExpired");
    case "invalid_credentials":
      return copy("genericSignInError");
    case "auth_unavailable":
      return copy("unavailableBody");
    default:
      return copy("unavailableBody");
  }
}
