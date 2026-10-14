import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider,
} from "@tanstack/react-router";
import { useMemo } from "react";
import { SessionProvider } from "../app/session";
import type { AuthProfile } from "../auth/authApi";
import type { Locale } from "../i18n";
import { Settings } from "../screens/Settings";

// Renders the Settings screen inside a real session and router, without the
// live work stream, for component tests of the account panels.

const root = createRootRoute({ component: Outlet });
const home = createRoute({ getParentRoute: () => root, path: "/" });
const settings = createRoute({
  getParentRoute: () => root,
  path: "/settings/$tab",
  component: function SettingsPage() {
    const { tab } = settings.useParams();
    return <Settings tab={tab} />;
  },
});
const tree = root.addChildren([home, settings]);

export function SettingsHarness({
  profile,
  csrf = "csrf",
  tab = "profile",
  onSessionLost = () => undefined,
}: {
  profile: AuthProfile;
  csrf?: string;
  tab?: string;
  onSessionLost?: (message: string | undefined, locale: Locale) => void;
}) {
  const router = useMemo(
    () =>
      createRouter({
        routeTree: tree,
        history: createMemoryHistory({ initialEntries: [`/settings/${tab}`] }),
      }),
    [tab],
  );
  return (
    <SessionProvider
      profile={profile}
      csrfToken={csrf}
      onSessionLost={onSessionLost}
    >
      <RouterProvider router={router} />
    </SessionProvider>
  );
}
