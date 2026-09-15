import { type ChainProgress } from '@keel/sdk';
import { useEffect, useState } from 'react';

// A type-only import, erased whole: an inline `type` specifier would keep
// `import {} from './chainWorker'` under verbatimModuleSyntax, and run the
// worker's module on the main thread.
import type { ChainMessage, ChainOutcome } from './chainWorker';

export type ChainCheck =
  | { status: 'checking'; progress: ChainProgress | undefined }
  | { status: 'done'; result: ChainOutcome }
  | { status: 'error'; message: string };

/**
 * The page's own check of a finished mission's chain (chainWorker.ts): the
 * progress while it runs, then every tick's recorded head, the recorded
 * decisions, and where the chain breaks, if it does. One worker per mission,
 * terminated on unmount.
 */
export function useChainCheck(mission: string): ChainCheck {
  const [check, setCheck] = useState<ChainCheck>({ status: 'checking', progress: undefined });

  useEffect(() => {
    const worker = new Worker(new URL('./chainWorker.ts', import.meta.url), { type: 'module' });
    worker.onmessage = (e: MessageEvent<ChainMessage>) => {
      const m = e.data;
      if (m.type === 'progress') setCheck({ status: 'checking', progress: m.progress });
      else if (m.type === 'done') setCheck({ status: 'done', result: m.result });
      else setCheck({ status: 'error', message: m.message });
    };
    worker.postMessage({ mission });
    return () => worker.terminate();
  }, [mission]);

  return check;
}
