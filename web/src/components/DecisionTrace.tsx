import { type Decision, type DecisionKind, type VectorID } from '@keel/sdk';
import { ScrollText, X } from 'lucide-react';
import { memo, useSyncExternalStore } from 'react';

import { PALETTE } from '@/cesium/missionLayers';
import { Button } from '@/components/ui/button';
import { missionTime } from '@/lib/format';
import { type MissionSource, type TraceEntry, useTrace } from '@/lib/missionStore';
import { shortHash } from '@/lib/planView';

import { SectionHeader } from './SectionHeader';

export interface DecisionTraceProps {
  store: MissionSource;
  selected: VectorID | undefined;
  /** Selects a vector, or clears the selection with undefined. */
  onSelect: (id: VectorID | undefined) => void;
}

const KIND_COLOUR: Record<DecisionKind, string> = {
  assignment: PALETTE.pale,
  reassignment: PALETTE.pale,
  redecompose: PALETTE.magenta,
  doctrine_rule: PALETTE.amber,
  doctrine_swap: PALETTE.cyan,
  mission_state: PALETTE.cyan,
  operator_notice: PALETTE.red,
};

/**
 * The engine's decisions as the stream delivered them, newest first, each
 * with its rationale and, opened, everything it recorded: the rule that
 * fired and the rules it shadowed, every vector considered with the reason
 * each was rejected, a hot swap's packs and windows. The operator's question
 * is "why didn't it do the other thing", and the answer is the rejected
 * candidates and the shadowed rules, so they are never left out.
 *
 * Where the stream's history starts, and where a resync may have lost
 * decisions, is marked (missionStore.ts). With a vector selected the trace
 * keeps to the decisions about it: those naming it as subject or winner.
 */
export function DecisionTrace({ store, selected, onSelect }: DecisionTraceProps) {
  const entries = useTrace(store);
  // A string, compared by value, so a tick that changes no vector re-renders nothing here.
  const known = useSyncExternalStore(store.subscribe, () => store.get()?.vectors.map((v) => v.caps.id).join(' ') ?? '');
  const vectors = new Set(known.split(' ').filter(Boolean));

  const shown = entries.filter((e) => !selected || !('decision' in e) || e.decision.subject === selected || e.decision.winner === selected);
  const decisions = shown.filter((e) => 'decision' in e).length;

  return (
    <section aria-label="Decisions" className="flex h-full min-h-0 flex-col">
      <SectionHeader icon={ScrollText} title="Decisions" count={decisions}>
        {selected && (
          <Button
            variant="ghost"
            size="xs"
            className="ml-auto font-mono normal-case"
            aria-label={`Show every decision, not only ${selected}'s`}
            onClick={() => onSelect(undefined)}
          >
            {selected} <X />
          </Button>
        )}
      </SectionHeader>
      <ol className="min-h-0 flex-1 overflow-y-auto">
        {shown
          .slice()
          .reverse()
          .map((entry) => (
            <li key={entry.key} className="border-b border-border/50">
              <Entry entry={entry} vectors={vectors} onSelect={onSelect} />
            </li>
          ))}
      </ol>
    </section>
  );
}

interface EntryProps {
  entry: TraceEntry;
  vectors: ReadonlySet<string>;
  onSelect: (id: VectorID) => void;
}

// Memoised on the entry, which never changes once traced: a tick adding a
// decision renders that one row, not the two thousand before it.
const Entry = memo(
  function Entry({ entry, vectors, onSelect }: EntryProps) {
    if (!('decision' in entry)) {
      return (
        <p className="px-4 py-1.5 font-mono text-[11px] text-muted-foreground italic">
          {missionTime(entry.t)}{' '}
          {entry.mark === 'connected'
            ? 'connected: decisions before this were not seen'
            : 'reconnected: decisions in between may be missing'}
        </p>
      );
    }
    return <DecisionView decision={entry.decision} vectors={vectors} onSelect={onSelect} />;
  },
  (a, b) => a.entry === b.entry && a.onSelect === b.onSelect && sameMembers(a.vectors, b.vectors),
);

function sameMembers(a: ReadonlySet<string>, b: ReadonlySet<string>): boolean {
  return a.size === b.size && [...a].every((x) => b.has(x));
}

function DecisionView({ decision: d, vectors, onSelect }: { decision: Decision; vectors: ReadonlySet<string>; onSelect: (id: VectorID) => void }) {
  const detailed = d.rule_fired || d.shadowed?.length || d.candidates?.length || d.swap;
  const head = (
    <span className="flex flex-wrap items-baseline gap-x-2 font-mono text-[11px]">
      <span className="text-muted-foreground">{missionTime(d.tick_ms)}</span>
      <span style={{ color: KIND_COLOUR[d.kind] }}>{d.kind}</span>
      {d.subject && <Name id={d.subject} vectors={vectors} onSelect={onSelect} />}
      {d.winner && d.winner !== d.subject && (
        <>
          <span className="text-muted-foreground">to</span>
          <Name id={d.winner} vectors={vectors} onSelect={onSelect} />
        </>
      )}
    </span>
  );

  // A bar in the kind's colour down the entry's edge: the trace reads as a
  // log of events, the roster above it as a list of vehicles.
  return (
    <div className="space-y-1 border-l-2 py-2 pr-4 pl-3.5 text-xs" style={{ borderLeftColor: KIND_COLOUR[d.kind] }}>
      {head}
      <p className="font-sans leading-snug">{d.rationale}</p>
      {detailed && (
        <details>
          <summary className="cursor-pointer text-[11px] text-muted-foreground">why</summary>
          <div className="mt-1 space-y-2 font-mono text-[11px]">
            {d.rule_fired && <p>rule {d.rule_fired} fired</p>}
            {d.shadowed?.length ? (
              <ul>
                {d.shadowed.map((s) => (
                  <li key={s.rule_id} className="text-muted-foreground">
                    {s.rule_id} (priority {s.priority}) shadowed by {s.shadowed_by}
                  </li>
                ))}
              </ul>
            ) : null}
            {d.candidates?.length ? (
              <table className="w-full">
                <tbody>
                  {d.candidates.map((c) => (
                    <tr key={c.vector} className={c.rejected ? 'text-muted-foreground' : ''}>
                      <td className="pr-2 align-top">{c.vector === d.winner ? '▸' : ''}</td>
                      <td className="pr-2 align-top">{c.vector}</td>
                      <td className="pr-2 text-right align-top">{c.rejected ? '' : Math.round(c.cost)}</td>
                      <td className="align-top">{c.reason ?? ''}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            ) : null}
            {d.swap && (
              <div className="space-y-0.5">
                <p>
                  {d.swap.from.name}@{d.swap.from.version} {shortHash(d.swap.from_hash)} to {d.swap.to.name}@{d.swap.to.version}{' '}
                  {shortHash(d.swap.to_hash)}
                </p>
                {d.swap.retained?.map((w) => (
                  <p key={`r:${w.rule_id}:${w.agent}`} className="text-muted-foreground">
                    kept {w.rule_id} for {w.agent} since {missionTime(w.since_ms)}
                  </p>
                ))}
                {d.swap.discarded?.map((w) => (
                  <p key={`d:${w.rule_id}:${w.agent}`} className="text-muted-foreground">
                    dropped {w.rule_id} for {w.agent}
                  </p>
                ))}
              </div>
            )}
          </div>
        </details>
      )}
    </div>
  );
}

function Name({ id, vectors, onSelect }: { id: string; vectors: ReadonlySet<string>; onSelect: (id: VectorID) => void }) {
  if (!vectors.has(id)) return <span>{id}</span>;
  return (
    <button type="button" className="underline decoration-dotted underline-offset-2 hover:text-foreground" onClick={() => onSelect(id)}>
      {id}
    </button>
  );
}
