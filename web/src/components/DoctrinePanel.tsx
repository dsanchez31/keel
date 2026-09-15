import { type DoctrineInfo, type KeelClient } from '@keel/sdk';
import { useMutation, useQuery } from '@tanstack/react-query';
import { Check, LoaderCircle, TriangleAlert } from 'lucide-react';
import { useState } from 'react';

import { Button } from '@/components/ui/button';
import { api } from '@/lib/api';
import { type Change, diffPacks, isEmpty } from '@/lib/doctrineDiff';
import { type MissionSource, useMissionView } from '@/lib/missionStore';
import { shortHash } from '@/lib/planView';
import { cn } from '@/lib/utils';

export interface DoctrinePanelProps {
  store: MissionSource;
  client?: KeelClient;
}

/** The query key of the registered packs, `GET /api/v1/doctrine`. */
const DOCTRINES_KEY = ['doctrines'] as const;

/**
 * The running mission's doctrine: the active pack by reference and by hash,
 * every registered pack to hot swap to, and what a swap would change, rule
 * by rule (doctrineDiff.ts). A swap is accepted, not applied: keeld applies
 * it at the next tick boundary and the doctrine_swap decision in the trace
 * says what happened to each duration window (spec section 6.6). Only a
 * running mission swaps.
 *
 * A registered pack of the active reference whose hash differs from the
 * running one is called out: the engine would refuse to swap to it, since a
 * pack is pinned by hash.
 */
export function DoctrinePanel({ store, client = api }: DoctrinePanelProps) {
  const view = useMissionView(store);
  const packs = useQuery({ queryKey: DOCTRINES_KEY, queryFn: ({ signal }) => client.doctrines({ signal }) });
  const [target, setTarget] = useState<string | undefined>(undefined);
  const swap = useMutation({
    mutationFn: ({ mission, pack }: { mission: string; pack: DoctrineInfo }) => client.swapDoctrine(mission, { name: pack.name, version: pack.version }),
  });

  const mission = view?.mission;
  const running = mission?.state === 'running';
  const activeRef = mission?.doctrine.name ? `${mission.doctrine.name}@${mission.doctrine.version}` : undefined;
  const activeHash = view?.doctrine_hash;
  const list = packs.data ?? [];
  const active = list.find((p) => p.hash === activeHash);
  const registered = list.find((p) => p.ref === activeRef);
  const chosen = list.find((p) => p.ref === target && p.hash !== activeHash);

  return (
    <div className="space-y-4 p-4 text-sm">
      <section className="space-y-1">
        <h3 className="font-mono text-xs tracking-widest text-muted-foreground uppercase">Active</h3>
        {activeRef && activeHash ? (
          <>
            <p className="font-mono">{activeRef}</p>
            <p className="font-mono text-[11px] break-all text-muted-foreground">{activeHash}</p>
            {registered && registered.hash !== activeHash && (
              <p className="flex gap-2 text-amber-400">
                <TriangleAlert className="mt-0.5 size-4 shrink-0" />
                The registry's {activeRef} has hash {shortHash(registered.hash)}, not the running one: it was edited under the same
                version, and the engine refuses to swap to it.
              </p>
            )}
          </>
        ) : (
          <p className="text-muted-foreground">No mission is running. A plan names its pack, pinned by hash when approved.</p>
        )}
      </section>

      <section className="space-y-1">
        <h3 className="font-mono text-xs tracking-widest text-muted-foreground uppercase">Registered packs</h3>
        {packs.isPending && <p className="text-muted-foreground">Loading</p>}
        {packs.error && <p className="text-destructive">{packs.error.message}</p>}
        <ul className="space-y-1">
          {list.map((p) => {
            const isActive = p.hash === activeHash;
            return (
              <li key={p.ref}>
                <button
                  type="button"
                  aria-pressed={p.ref === target}
                  disabled={isActive}
                  onClick={() => {
                    swap.reset();
                    setTarget(p.ref === target ? undefined : p.ref);
                  }}
                  className={cn(
                    'flex w-full items-baseline justify-between gap-2 rounded-md border px-2 py-1.5 text-left font-mono text-xs hover:bg-muted/50 disabled:cursor-default disabled:hover:bg-transparent',
                    p.ref === target && 'border-cyan-400/60 bg-muted',
                  )}
                >
                  <span>{p.ref}</span>
                  <span className="text-muted-foreground">{isActive ? 'active' : shortHash(p.hash)}</span>
                </button>
              </li>
            );
          })}
        </ul>
      </section>

      {chosen && (
        <section className="space-y-2">
          <h3 className="font-mono text-xs tracking-widest text-muted-foreground uppercase">
            {active ? `${active.ref} to ${chosen.ref}` : `To ${chosen.ref}`}
          </h3>
          {active ? <DiffView from={active} to={chosen} /> : <p className="text-muted-foreground">The active pack is not registered: no diff.</p>}
          <Button
            className="w-full"
            disabled={!running || swap.isPending}
            onClick={() => {
              if (mission) swap.mutate({ mission: mission.id, pack: chosen });
            }}
          >
            {swap.isPending ? <LoaderCircle className="animate-spin" /> : null} Swap at the next tick
          </Button>
          {!running && <p className="text-xs text-muted-foreground">Only a running mission swaps its pack.</p>}
          {swap.isSuccess && (
            <p className="flex gap-2 text-xs">
              <Check className="size-4 shrink-0 text-emerald-400" /> Accepted. The doctrine_swap decision in the trace records the windows
              kept and dropped.
            </p>
          )}
          {swap.error && (
            <p role="alert" className="text-xs text-destructive">
              {swap.error.message}
            </p>
          )}
        </section>
      )}
    </div>
  );
}

function DiffView({ from, to }: { from: DoctrineInfo; to: DoctrineInfo }) {
  const diff = diffPacks(from.document, to.document);
  if (isEmpty(diff)) return <p className="text-muted-foreground">Same rules, constraints, roles and parameters.</p>;
  return (
    <div className="space-y-3 font-mono text-[11px]">
      <DiffSection title="Parameters" changes={diff.params} show={(v) => String(v)} />
      <DiffSection title="Roles" changes={diff.roles} show={(r) => `requires ${r.requires.join(', ')}`} />
      <DiffSection title="Constraints" changes={diff.constraints} show={(c) => c.rule} />
      <DiffSection title="Rules" changes={diff.rules} show={(r) => `when ${r.when} then ${r.then} (priority ${r.priority})`} />
      {diff.rules.some((c) => c.change !== 'added') && (
        <p className="font-sans text-xs text-muted-foreground">
          A rule the new pack keeps by id, changed or not, keeps its duration windows; a removed rule's windows are dropped.
        </p>
      )}
    </div>
  );
}

function DiffSection<T>({ title, changes, show }: { title: string; changes: readonly Change<T>[]; show: (t: T) => string }) {
  if (changes.length === 0) return null;
  return (
    <div className="space-y-1">
      <p className="font-sans text-xs text-muted-foreground">{title}</p>
      <ul className="space-y-1">
        {changes.map((c) => (
          <li key={c.id} className="rounded-md bg-background p-1.5 break-words">
            <span className="text-foreground">{c.id}</span>
            {c.change !== 'added' && <p className="text-red-400">- {show(c.from)}</p>}
            {c.change !== 'removed' && <p className="text-emerald-400">+ {show(c.to)}</p>}
          </li>
        ))}
      </ul>
    </div>
  );
}
