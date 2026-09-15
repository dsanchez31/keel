import { QueryClient } from '@tanstack/react-query';

/**
 * The one query cache. Queries serve keeld's REST side (packs, plans, finished
 * missions); the live mission comes over the WebSocket, not through here.
 */
export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // keeld is local and answers or does not: one retry, then the screen
      // says so, rather than three silent ones.
      retry: 1,
      // A tab regaining focus is no reason to refetch what the stream keeps
      // current.
      refetchOnWindowFocus: false,
    },
  },
});
