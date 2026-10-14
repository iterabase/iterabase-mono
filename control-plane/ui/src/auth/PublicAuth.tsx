import {
  FormEvent,
  ReactNode,
  RefObject,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { AuthAPIError, AuthProfile, LocaleCode, authApi } from "./authApi";
import type { TFunction } from "i18next";
import { fixedT, setLocale as setActiveLocale } from "../i18n";
import { AuthFrame } from "./AuthFrame";

// The signed-out journey (HOR-453), ported from XTSignIn / XTPhoneSignIn: every
// view (sign in, request access, forgot password, verify, setup, reset and
// their terminal states) sits in the same right-hand card. Single sign-on
// buttons join with HOR-314.

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
  /** The installation's name (DES-HOR-459-08); "" shows no eyebrow. */
  organizationName?: string;
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
  organizationName = "",
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

  // Each view change moves focus to its heading, so screen readers hear it.
  useEffect(() => {
    headingRef.current?.focus();
  }, [view.kind]);

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

  const go = (kind: "sign-in" | "request" | "forgot") => {
    setError("");
    setNotice("");
    setView({ kind });
  };
  const back = (
    <button
      type="button"
      className="link-button self-start"
      onClick={() => go("sign-in")}
    >
      {c("backToSignIn")}
    </button>
  );

  return (
    <AuthFrame
      brand={c("brand")}
      eyebrow={organizationName}
      title={c("heroTitle")}
      subline={c("heroSub")}
      trust={[c("trustApproval"), c("trustAttributed"), c("trustData")]}
      locale={
        <div
          className="seg auth-locale"
          role="group"
          aria-label={c("language")}
        >
          {(["en", "pt"] as const).map((code) => (
            <button
              key={code}
              type="button"
              aria-pressed={locale === code}
              onClick={() => setLocale(code)}
            >
              {c(`languageShort.${code}`)}
            </button>
          ))}
        </div>
      }
    >
      {view.kind === "sign-in" && (
        <form className="auth-form" onSubmit={onSignIn} aria-busy={busy}>
          <Title
            headingRef={headingRef}
            className="auth-card-title--phone-hidden"
          >
            {c("signInTitle")}
          </Title>
          <Feedback error={error} notice={notice} />
          <EmailField copy={c} value={email} onChange={setEmail} sign />
          <label className="field">
            <span>{c("password")}</span>
            <input
              type="password"
              autoComplete="current-password"
              required
              value={password}
              onChange={(event) => setPassword(event.target.value)}
            />
          </label>
          <button
            type="submit"
            className="btn btn-ink btn-lg btn-full"
            disabled={busy}
          >
            {busy ? c("signingIn") : c("signIn")}
          </button>
          <div className="auth-links">
            <button
              type="button"
              className="link-button"
              onClick={() => go("forgot")}
            >
              {c("forgotPassword")}
            </button>
            <button
              type="button"
              className="link-button"
              onClick={() => go("request")}
            >
              {c("requestAccessLink")}
            </button>
          </div>
        </form>
      )}

      {view.kind === "request" && (
        <form className="auth-form" onSubmit={onRequestAccess} aria-busy={busy}>
          <Title headingRef={headingRef}>{c("requestTitle")}</Title>
          <p className="auth-intro">{c("requestIntro")}</p>
          <Feedback error={error} notice={notice} />
          <EmailField copy={c} value={email} onChange={setEmail} />
          <label className="field">
            <span>{c("language")}</span>
            <select
              value={locale}
              onChange={(event) => setLocale(event.target.value as LocaleCode)}
            >
              <option value="en">{c("languageName.en")}</option>
              <option value="pt">{c("languageName.pt")}</option>
            </select>
          </label>
          <button
            type="submit"
            className="btn btn-ink btn-lg btn-full"
            disabled={busy}
          >
            {busy ? c("requestSubmitting") : c("requestSubmit")}
          </button>
          {back}
        </form>
      )}

      {view.kind === "forgot" && (
        <form className="auth-form" onSubmit={onForgot} aria-busy={busy}>
          <Title headingRef={headingRef}>{c("forgotTitle")}</Title>
          <p className="auth-intro">{c("forgotIntro")}</p>
          <Feedback error={error} notice={notice} />
          <EmailField copy={c} value={email} onChange={setEmail} />
          <button
            type="submit"
            className="btn btn-ink btn-lg btn-full"
            disabled={busy}
          >
            {c("forgotSubmit")}
          </button>
          {back}
        </form>
      )}

      {view.kind === "request-accepted" && (
        <State headingRef={headingRef} title={c("requestTitle")}>
          <p className="auth-intro">{c("requestAccepted")}</p>
          {back}
        </State>
      )}

      {view.kind === "forgot-accepted" && (
        <State headingRef={headingRef} title={c("forgotTitle")}>
          <p className="auth-intro">{c("forgotAccepted")}</p>
          {back}
        </State>
      )}

      {view.kind === "verify-checking" && (
        <div className="auth-state" aria-busy="true">
          <Title headingRef={headingRef}>{c("verifyChecking")}</Title>
          <p className="auth-intro">{c("loading")}</p>
        </div>
      )}

      {view.kind === "verify-error" && (
        <State headingRef={headingRef} title={verifyErrorTitle(view.reason, c)}>
          <p className="auth-intro">
            {view.reason === "expired" || view.reason === "superseded"
              ? c("verifyExpired")
              : c("verifyInvalid")}
          </p>
          <div className="auth-links">
            <button
              type="button"
              className="link-button"
              onClick={() => go("request")}
            >
              {c("requestAccessLink")}
            </button>
            <button
              type="button"
              className="link-button"
              onClick={() => go("sign-in")}
            >
              {c("backToSignIn")}
            </button>
          </div>
        </State>
      )}

      {view.kind === "request-state" && (
        <State headingRef={headingRef} title={requestStateTitle(view.state, c)}>
          <p className="auth-intro">{requestStateBody(view.state, c)}</p>
          <button
            type="button"
            className="btn btn-secondary btn-full"
            onClick={() => go("sign-in")}
          >
            {c("backToSignIn")}
          </button>
        </State>
      )}

      {view.kind === "setup" && (
        <form className="auth-form" onSubmit={onSetup} aria-busy={busy}>
          <Title headingRef={headingRef}>{c("setupTitle")}</Title>
          <p className="auth-intro">{c("setupIntro")}</p>
          <Feedback error={error} notice={notice} />
          {contextLoading && (
            <p className="auth-intro" role="status">
              {c("loading")}
            </p>
          )}
          {setupContext && (
            <>
              <label className="field">
                <span>{c("email")}</span>
                <input value={setupContext.email} readOnly disabled />
              </label>
              <label className="field">
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
          <label className="field">
            <span>{c("displayName")}</span>
            <input
              required
              autoComplete="name"
              value={displayName}
              onChange={(event) => setDisplayName(event.target.value)}
            />
          </label>
          <label className="field">
            <span>{c("language")}</span>
            <select
              value={locale}
              onChange={(event) => setLocale(event.target.value as LocaleCode)}
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
          <button
            type="submit"
            className="btn btn-ink btn-lg btn-full"
            disabled={busy}
          >
            {busy ? c("setupCompleting") : c("finishSetup")}
          </button>
        </form>
      )}

      {view.kind === "setup-terminal" && (
        <State headingRef={headingRef} title={setupTitle(view.reason, c)}>
          <Feedback error={error} notice={notice} />
          <p className="auth-intro">{setupBody(view.reason, c)}</p>
          <button
            type="button"
            className="btn btn-ink btn-full"
            disabled={busy}
            onClick={onResendSetup}
          >
            {c("setupResend")}
          </button>
          {back}
        </State>
      )}

      {view.kind === "setup-done" && (
        <State headingRef={headingRef} title={c("setupDoneTitle")}>
          <p className="auth-intro">{c("setupDoneBody")}</p>
          <button
            type="button"
            className="btn btn-ink btn-lg btn-full"
            onClick={() => go("sign-in")}
          >
            {c("signIn")}
          </button>
        </State>
      )}

      {view.kind === "reset" && (
        <form className="auth-form" onSubmit={onReset} aria-busy={busy}>
          <Title headingRef={headingRef}>{c("resetTitle")}</Title>
          <p className="auth-intro">{c("resetIntro")}</p>
          <Feedback error={error} notice={notice} />
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
          <button
            type="submit"
            className="btn btn-ink btn-lg btn-full"
            disabled={busy}
          >
            {busy ? c("resetCompleting") : c("resetSubmit")}
          </button>
        </form>
      )}

      {view.kind === "reset-terminal" && (
        <State headingRef={headingRef} title={resetTitle(view.reason, c)}>
          <p className="auth-intro">{resetBody(view.reason, c)}</p>
          {back}
        </State>
      )}

      {view.kind === "reset-done" && (
        <State headingRef={headingRef} title={c("resetDoneTitle")}>
          <p className="auth-intro">{c("resetDoneBody")}</p>
          <button
            type="button"
            className="btn btn-ink btn-lg btn-full"
            onClick={() => go("sign-in")}
          >
            {c("signIn")}
          </button>
        </State>
      )}

      {/* Errors outside a form view (e.g. a lost session on a terminal
          state) still reach the person. */}
      {!formViews.has(view.kind) && view.kind !== "setup-terminal" && (
        <Feedback error={error} notice={notice} />
      )}
    </AuthFrame>
  );
}

const formViews = new Set<View["kind"]>([
  "sign-in",
  "request",
  "forgot",
  "setup",
  "reset",
]);

function Title({
  headingRef,
  className = "",
  children,
}: {
  headingRef: RefObject<HTMLHeadingElement | null>;
  className?: string;
  children: ReactNode;
}) {
  return (
    <h2
      ref={headingRef}
      tabIndex={-1}
      className={`auth-card-title ${className}`}
    >
      {children}
    </h2>
  );
}

function State({
  headingRef,
  title,
  children,
}: {
  headingRef: RefObject<HTMLHeadingElement | null>;
  title: string;
  children: ReactNode;
}) {
  return (
    <div className="auth-state" role="status">
      <Title headingRef={headingRef}>{title}</Title>
      {children}
    </div>
  );
}

function Feedback({ error, notice }: { error: string; notice: string }) {
  return (
    <>
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
    </>
  );
}

function EmailField({
  copy,
  value,
  onChange,
  sign = false,
}: {
  copy: AuthT;
  value: string;
  onChange: (value: string) => void;
  sign?: boolean;
}) {
  return (
    <label className="field">
      <span>{copy("email")}</span>
      <input
        type="email"
        autoComplete={sign ? "username" : "email"}
        required
        value={value}
        onChange={(event) => onChange(event.target.value)}
      />
    </label>
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
      <label className="field">
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
      <label className="field">
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
        <p className="form-error m-0" role="alert">
          {mismatch}
        </p>
      )}
      <p id="password-guidance" className="auth-guidance">
        {copy("passwordGuidance")}
      </p>
    </>
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
