import { createClient } from '@keel/sdk';

/**
 * keeld's REST API at `/api/v1` of the page's own origin: the dev server
 * proxies it (vite.config.ts), a deployment serves both from one origin, so
 * keeld's cross-origin guard sees same-origin writes.
 */
export const api = createClient();
