import { RouterProvider, type RouterHistory } from "@tanstack/react-router";
import { useCallback, useEffect, useMemo, useState } from "react";
import { LiveProvider } from "./app/live";
import { createAppRouter } from "./app/router";
import { SessionProvider } from "./app/session";
import { AuthProfile, probeSession } from "./auth/authApi";
import PublicAuth from "./auth/PublicAuth";
import { browserLocale, fixedT, type Locale } from "./i18n";

// The customer app (HOR-459): the signed-out journey until a browser session
// exists (HOR-582), then the routed app. Losing the session unmounts every
// protected screen and returns to sign-in.

type AuthState =
  | { kind: "probing" }
  | { kind: "anonymous"; message?: string; locale?: Locale }
  | { kind: "authenticated"; profile: AuthProfile; csrfToken: string }
  | { kind: "unavailable" };

export default function App({ history }: { history?: RouterHistory }) {
  const [auth, setAuth] = useState<AuthState>({ kind: "probing" });

  const probe = useCallback(() => {
    let cancelled = false;
    void probeSession().then((result) => {
      if (cancelled) return;
      if (result.kind === "authenticated")
        setAuth({
          kind: "authenticated",
          profile: result.profile,
          csrfToken: result.csrfToken,
        });
      else if (result.kind === "anonymous")
        setAuth({ kind: "anonymous", locale: result.locale });
      else setAuth({ kind: "unavailable" });
    });
    return () => {
      cancelled = true;
    };
  }, []);
  useEffect(probe, [probe]);

  const onSessionLost = useCallback(
    (message: string | undefined, locale: Locale) => {
      setAuth({
        kind: "anonymous",
        locale,
        message:
          message === "sessionExpired"
            ? fixedT(locale, "common")("sessionExpired")
            : message,
      });
    },
    [],
  );

  if (auth.kind === "probing")
    return (
      <div className="auth-page">
        <div className="auth-card" role="status" aria-busy="true">
          {fixedT(browserLocale(), "common")("loading")}
        </div>
      </div>
    );
  if (auth.kind === "anonymous")
    return (
      <PublicAuth
        initialMessage={auth.message}
        initialLocale={auth.locale ?? browserLocale()}
        onAuthenticated={(profile, csrfToken) =>
          setAuth({ kind: "authenticated", profile, csrfToken })
        }
      />
    );
  if (auth.kind === "unavailable") {
    const copy = fixedT(browserLocale(), "auth");
    return (
      <main className="auth-page">
        <section className="auth-card">
          <h1>{copy("unavailableTitle")}</h1>
          <p>{copy("unavailableBody")}</p>
          <button
            type="button"
            className="ib-btn ib-btn--primary ib-btn--md"
            onClick={() => {
              setAuth({ kind: "probing" });
              probe();
            }}
          >
            {copy("retry")}
          </button>
        </section>
      </main>
    );
  }
  return (
    <SessionProvider
      profile={auth.profile}
      csrfToken={auth.csrfToken}
      onSessionLost={onSessionLost}
    >
      <LiveProvider>
        <RoutedApp history={history} />
      </LiveProvider>
    </SessionProvider>
  );
}

function RoutedApp({ history }: { history?: RouterHistory }) {
  const router = useMemo(() => createAppRouter(history), [history]);
  return <RouterProvider router={router} />;
}
