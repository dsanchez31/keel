import { getRouteApi } from '@tanstack/react-router';

import { ReplayScreen } from './ReplayScreen';

// Typed through the registered router, without importing the route whose
// component this is.
const replayRoute = getRouteApi('/replays/$id');

export function ReplayRoute() {
  const { id } = replayRoute.useParams();
  // Keyed by mission: another id starts from a clean store, playhead and
  // chain check rather than inheriting the last one's.
  return <ReplayScreen key={id} mission={id} />;
}
