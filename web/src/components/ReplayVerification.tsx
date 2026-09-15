import { headAt, type ReplayDivergence, type ReplayEnd } from '@keel/sdk';
import { Check, LoaderCircle, TriangleAlert, X } from 'lucide-react';
import { type ReactNode, useSyncExternalStore } from 'react';

import { missionTime } from '@/lib/format';
import { type MissionSource } from '@/lib/missionStore';
import { type ChainCheck } from '@/lib/useChainCheck';

/** What keeld's replay windows found, gathered across the windows loaded. */
export interface ReplayVerdict {
  verifiedMs: number | undefined;
  divergence: ReplayDivergence | undefined;
  end: ReplayEnd | undefined;
}

export interface ReplayVerificationProps {
  mission: string;
  store: MissionSource;
  check: ChainCheck;
  verdict: ReplayVerdict;
  /** Why keeld refused the last window, if it did. */
  windowError?: string;
}

/**
 * The replay's proof, from two sides that do not trust each other:
 *
 * - the chain as this page recomputed it from the log's bytes (I6,
 *   chainWorker.ts), every tick's recorded head;
 * - the chain keeld rebuilt by running the engine over the log (I7), as far
 *   as its windows have gone, and where it diverged if it did.
 *
 * At the playhead the two heads of the same tick sit side by side; at the
 * end, the page's head, keeld's recorded head and keeld's rebuilt head. Three
 * equal values are the determinism claim, checked on screen.
 */
export function ReplayVerification({ mission, store, check, verdict, windowError }: ReplayVerificationProps) {
  // A string compared by value: renders on a tick boundary of the view only.
  const at = useSyncExternalStore(store.subscribe, () => {
    const v = store.get();
    return v ? `${v.tick_ms} ${v.head}` : '';
  });
  const [tickText, rebuilt] = at.split(' ');
  const tickMs = tickText ? Number(tickText) : undefined;
  const result = check.status === 'done' ? check.result : undefined;
  const recorded = result && tickMs !== undefined ? headAt(result.ticks, tickMs) : undefined;
  const sameTick = recorded !== undefined && recorded.tickMs === tickMs;
  const end = verdict.end;
  const reproduced = !!end && !!result && !result.broken && end.recorded === end.replayed && end.recorded === result.head;

  return (
    <aside aria-label="Verification" className="flex h-full min-h-0 flex-col overflow-y-auto border-l bg-card p-4 text-sm text-card-foreground">
      <h2 className="font-mono text-xs tracking-widest text-muted-foreground uppercase">Replay {mission}</h2>

      {reproduced && end && (
        <p className="mt-3 flex gap-2 rounded-md border border-emerald-400/40 bg-emerald-400/10 p-2">
          <Check className="mt-0.5 size-4 shrink-0 text-emerald-400" />
          <span>
            The replay reproduces the recording: one head, <span className="font-mono break-all">{end.replayed}</span>
          </span>
        </p>
      )}

      <Section title="Chain, checked by this page">
        {check.status === 'checking' && (
          <Line icon={<LoaderCircle className="size-4 animate-spin" />}>
            {check.progress ? `${check.progress.records.toLocaleString()} records linked, at ${missionTime(check.progress.tickMs)}` : 'Reading the log'}
          </Line>
        )}
        {check.status === 'error' && <Line icon={<TriangleAlert className="size-4 text-destructive" />}>{check.message}</Line>}
        {result &&
          (result.broken ? (
            <Line icon={<X className="size-4 text-destructive" />}>
              Broken at record {result.broken.seq}: {result.broken.reason}
            </Line>
          ) : (
            <Line icon={<Check className="size-4 text-emerald-400" />}>
              {result.records.toLocaleString()} records linked (I6), head <Hash value={result.head} />
            </Line>
          ))}
      </Section>

      <Section title="Replay, rebuilt by keeld">
        {windowError && <Line icon={<TriangleAlert className="size-4 text-destructive" />}>{windowError}</Line>}
        {verdict.divergence ? (
          <Line icon={<X className="size-4 text-destructive" />}>
            Diverged at record {verdict.divergence.seq}, {missionTime(verdict.divergence.tick_ms)}: recorded <Hash value={verdict.divergence.recorded} />
            , rebuilt <Hash value={verdict.divergence.replayed} />
          </Line>
        ) : verdict.verifiedMs !== undefined ? (
          <Line icon={<Check className="size-4 text-emerald-400" />}>
            Every record rebuilt to its recorded digest through {missionTime(verdict.verifiedMs)} (I7)
          </Line>
        ) : (
          !windowError && <Line icon={<LoaderCircle className="size-4 animate-spin" />}>Replaying</Line>
        )}
      </Section>

      <Section title={tickMs === undefined ? 'At the playhead' : `At ${missionTime(tickMs)}`}>
        <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 font-mono text-[11px]">
          <dt className="text-muted-foreground">rebuilt</dt>
          <dd className="break-all">{rebuilt ?? ''}</dd>
          <dt className="text-muted-foreground">recorded</dt>
          <dd className="break-all">{sameTick ? recorded.head : ''}</dd>
        </dl>
        {sameTick && rebuilt && (
          <Line icon={recorded.head === rebuilt ? <Check className="size-4 text-emerald-400" /> : <X className="size-4 text-destructive" />}>
            {recorded.head === rebuilt ? 'Same head' : 'The heads differ'}
          </Line>
        )}
      </Section>

      {end && (
        <Section title={`At the end, ${missionTime(end.tick_ms)}`}>
          <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 font-mono text-[11px]">
            <dt className="text-muted-foreground">page</dt>
            <dd className="break-all">{result?.head ?? ''}</dd>
            <dt className="text-muted-foreground">recorded</dt>
            <dd className="break-all">{end.recorded}</dd>
            <dt className="text-muted-foreground">rebuilt</dt>
            <dd className="break-all">{end.replayed}</dd>
          </dl>
        </Section>
      )}
    </aside>
  );
}

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="mt-4 space-y-2">
      <h3 className="font-mono text-xs tracking-widest text-muted-foreground uppercase">{title}</h3>
      {children}
    </section>
  );
}

function Line({ icon, children }: { icon: ReactNode; children: ReactNode }) {
  return (
    <p className="flex gap-2 text-xs">
      <span className="mt-0.5 shrink-0">{icon}</span>
      <span>{children}</span>
    </p>
  );
}

function Hash({ value }: { value: string | undefined }) {
  return (
    <span className="font-mono" title={value}>
      {value ? value.slice(0, 12) : 'none'}
    </span>
  );
}
