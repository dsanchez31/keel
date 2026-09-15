import { createRootRoute, createRoute, createRouter } from '@tanstack/react-router';

import { MissionScreen } from './MissionScreen';
import { NotFound } from './NotFound';
import { ReplayRoute } from './ReplayRoute';
import { RootLayout } from './RootLayout';

/**
 * The route tree, declared in code: KEEL has a handful of screens, so the
 * generated tree of file-based routing buys nothing.
 */
const rootRoute = createRootRoute({
  component: RootLayout,
  notFoundComponent: NotFound,
});

const missionRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/',
  component: MissionScreen,
});

const replayRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/replays/$id',
  component: ReplayRoute,
});

const routeTree = rootRoute.addChildren([missionRoute, replayRoute]);

export const router = createRouter({ routeTree, defaultPreload: 'intent' });

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router;
  }
}
