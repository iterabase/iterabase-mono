import {
  createRootRoute,
  createRoute,
  createRouter,
  redirect,
  useNavigate,
  type RouterHistory,
} from "@tanstack/react-router";
import { NotFound } from "../components/states";
import {
  Coworker,
  type ActivityFilter,
  type CoworkerTab,
} from "../screens/Coworker";
import { More } from "../screens/More";
import { Settings } from "../screens/Settings";
import { Team } from "../screens/Team";
import { Today } from "../screens/Today";
import { Work } from "../screens/Work";
import { WorkItemSheet } from "../screens/WorkItem";
import { YourTurn } from "../screens/YourTurn";
import type { PeriodPreset } from "../work/model";
import { Shell } from "./Shell";

// The app's routes (TanStack Router). Every screen has a URL so deep links,
// refresh and the back button work; the Go handler serves index.html for
// these paths (DES-HOR-459-01).

function oneOf<T extends string>(
  value: unknown,
  allowed: readonly T[],
  fallback: T,
): T {
  return allowed.includes(value as T) ? (value as T) : fallback;
}
const periods = ["today", "week", "month"] as const;

const rootRoute = createRootRoute({
  component: Shell,
  notFoundComponent: NotFound,
});

const todayRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/",
  component: Today,
});

const yourTurnRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/your-turn",
  component: YourTurn,
});

const workRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/work",
  validateSearch: (
    search: Record<string, unknown>,
  ): { period?: PeriodPreset } =>
    search.period === undefined
      ? {}
      : { period: oneOf(search.period, periods, "today") },
  component: function WorkPage() {
    const { period } = workRoute.useSearch();
    return <Work period={period ?? "today"} />;
  },
});

const workItemRoute = createRoute({
  getParentRoute: () => workRoute,
  path: "$itemId",
  component: function WorkItemPage() {
    const { itemId } = workItemRoute.useParams();
    const search = workRoute.useSearch();
    const navigate = useNavigate();
    return (
      <WorkItemSheet
        itemId={itemId}
        onClose={() => void navigate({ to: "/work", search })}
      />
    );
  },
});

const teamRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/team",
  component: Team,
});

const coworkerRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/team/$key",
  validateSearch: (
    search: Record<string, unknown>,
  ): { tab?: CoworkerTab; filter?: ActivityFilter; period?: PeriodPreset } => ({
    ...(search.tab !== undefined && {
      tab: oneOf(
        search.tab,
        ["how", "can", "activity", "failed"] as const,
        "how",
      ),
    }),
    ...(search.filter !== undefined && {
      filter: oneOf(
        search.filter,
        ["all", "needs", "done", "failed"] as const,
        "all",
      ),
    }),
    ...(search.period !== undefined && {
      period: oneOf(search.period, periods, "week"),
    }),
  }),
  component: function CoworkerPage() {
    const { key } = coworkerRoute.useParams();
    const search = coworkerRoute.useSearch();
    return (
      <Coworker
        workflowKey={key}
        tab={search.tab ?? "how"}
        filter={search.filter ?? "all"}
        period={search.period ?? "week"}
      />
    );
  },
});

const settingsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/settings",
  beforeLoad: () => {
    throw redirect({ to: "/settings/$tab", params: { tab: "profile" } });
  },
});

const settingsTabRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/settings/$tab",
  component: function SettingsPage() {
    const { tab } = settingsTabRoute.useParams();
    return <Settings tab={tab} />;
  },
});

const moreRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/more",
  component: More,
});

// Email links (/auth/verify, /auth/setup, /auth/reset) belong to the signed-out
// journey; a signed-in person following one lands on Today.
const authRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/auth/$",
  beforeLoad: () => {
    throw redirect({ to: "/" });
  },
});

const routeTree = rootRoute.addChildren([
  todayRoute,
  yourTurnRoute,
  workRoute.addChildren([workItemRoute]),
  teamRoute,
  coworkerRoute,
  settingsRoute,
  settingsTabRoute,
  moreRoute,
  authRoute,
]);

export function createAppRouter(history?: RouterHistory) {
  return createRouter({
    routeTree,
    history,
    defaultPreload: false,
    scrollRestoration: true,
  });
}

declare module "@tanstack/react-router" {
  interface Register {
    router: ReturnType<typeof createAppRouter>;
  }
}
