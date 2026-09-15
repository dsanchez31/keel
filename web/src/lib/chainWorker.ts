import { type ChainProgress, type ChainResult, type Decision, lines, verifyChain } from '@keel/sdk';

/**
 * Checks a finished mission's chain off the main thread: a long mission is
 * some hundred thousand records, each a SHA-256, and the globe keeps drawing
 * meanwhile. Started by useChainCheck with the mission's id; streams the log
 * from keeld's own origin and posts progress, then the result or the error.
 *
 * It keeps the recorded decisions on the way, the replay's decision trace:
 * the recording's own, complete from the first tick, where a replay window
 * starts at its snapshot.
 */

export type ChainRequest = { mission: string };
export type ChainOutcome = ChainResult & { decisions: Decision[] };
export type ChainMessage =
  | { type: 'progress'; progress: ChainProgress }
  | { type: 'done'; result: ChainOutcome }
  | { type: 'error'; message: string };

// The file is a dedicated worker's module, but the app's TypeScript program
// carries the DOM library, where `self` is a Window; this is the part of the
// worker scope used here.
const scope = self as unknown as {
  onmessage: ((e: MessageEvent<ChainRequest>) => void) | null;
  postMessage(message: ChainMessage): void;
};

scope.onmessage = (e) => {
  void check(e.data.mission).then(
    (result) => scope.postMessage({ type: 'done', result }),
    (err: unknown) => scope.postMessage({ type: 'error', message: err instanceof Error ? err.message : String(err) }),
  );
};

async function check(mission: string): Promise<ChainOutcome> {
  const res = await fetch(`/api/v1/replays/${encodeURIComponent(mission)}`);
  if (!res.ok || !res.body) throw new Error(`keeld answered ${res.status} for the log of ${mission}`);
  const decisions: Decision[] = [];
  const result = await verifyChain(lines(res.body), {
    onProgress: (progress) => scope.postMessage({ type: 'progress', progress }),
    onRecord: (r) => {
      if (r.kind === 'decision') decisions.push(JSON.parse(r.payload) as Decision);
    },
  });
  return { ...result, decisions };
}
