import { type ApprovedPlan, type AreaView, type Attempt, type Outcome } from '@keel/sdk';
import { Check, LoaderCircle, TriangleAlert, X } from 'lucide-react';
import { type ReactNode, useRef } from 'react';

import { Button } from '@/components/ui/button';
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs';
import { expandedSource, irSource, laneRows, shortHash } from '@/lib/planView';
import { type PlanGate } from '@/lib/usePlanGate';
import { exampleIntent, type World } from '@/lib/useWorld';

import { IntentBar, type IntentBarHandle } from './IntentBar';
import { WorldNames } from './WorldNames';

export interface AssistantPanelProps {
  gate: PlanGate;
  /** The world's areas and stations, listed above the intent bar. */
  world?: World;
  /** An area picked in the list: the globe shows it. */
  onArea?: (area: AreaView) => void;
}

/**
 * The assistant, first tab of the right column (ControlColumn.tsx): intent
 * in, a plan out, and the launch gate between them (the fence's human gate,
 * spec section 8.1). The flow design section 8.3 borrows from LE_VECTOR's
 * assistant panel: type intent at the bottom, read the compiled plan and the
 * model's rationale above it, approve or discard. Nothing moves before approval; a compilation the validator
 * refused three times shows every diagnostic and offers nothing to approve,
 * which is the correct outcome, not an error. An intent the triage found no
 * mission in shows the model's reason and what the assistant takes instead.
 * The names an intent may use are listed above the bar (WorldNames).
 */
export function AssistantPanel({ gate, world, onArea }: AssistantPanelProps) {
  const { outcome, approval, error, busy } = gate;
  const bar = useRef<IntentBarHandle>(null);
  const example = exampleIntent(world?.view);

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="min-h-0 flex-1 space-y-4 overflow-y-auto p-4 text-sm">
        {!outcome && !approval && !error && busy !== 'compile' && (
          <p className="font-sans text-muted-foreground">
            State the mission in plain language, naming one of the areas listed below. The planner proposes a plan, the validator
            checks it, and nothing moves until you approve it.
          </p>
        )}
        {busy === 'compile' && (
          <p className="flex items-center gap-2 text-muted-foreground">
            <LoaderCircle className="size-4 animate-spin" /> Compiling intent
          </p>
        )}
        {error && (
          <p role="alert" className="flex gap-2 text-destructive">
            <TriangleAlert className="mt-0.5 size-4 shrink-0" /> {error.message}
          </p>
        )}
        {approval && (
          <p className="flex gap-2">
            <Check className="mt-0.5 size-4 shrink-0 text-emerald-400" />
            <span>
              Approved as <span className="font-mono">{approval.mission}</span>, plan{' '}
              <span className="font-mono">{shortHash(approval.plan)}</span>. The engine starts it at its next tick; a refusal shows in
              the decision trace.
            </span>
          </p>
        )}
        {outcome && <OutcomeView outcome={outcome} example={example} />}
      </div>
      {outcome?.plan && (
        <div className="flex gap-2 border-t px-4 py-3">
          <Button className="flex-1" onClick={gate.approve} disabled={busy !== undefined}>
            {busy === 'approve' ? <LoaderCircle className="animate-spin" /> : <Check />} Approve and launch
          </Button>
          <Button variant="outline" onClick={gate.discard} disabled={busy !== undefined}>
            <X /> Discard
          </Button>
        </div>
      )}
      <footer className="space-y-2 border-t p-3">
        {world && <WorldNames world={world} onPick={(name) => bar.current?.insert(name)} onArea={onArea} />}
        <IntentBar ref={bar} onCompile={gate.compile} compiling={busy === 'compile'} placeholder={example} />
      </footer>
    </div>
  );
}

function OutcomeView({ outcome, example }: { outcome: Outcome; example: string }) {
  const { plan, ir, attempts, triage } = outcome;
  const refused = attempts.filter((a) => a.diagnostics?.length);

  if (triage && !triage.mission) {
    return (
      <div className="space-y-4">
        <blockquote className="border-l-2 border-cyan-400/60 pl-3 font-sans text-muted-foreground italic">{outcome.intent}</blockquote>
        <div className="space-y-2 text-amber-400">
          <p className="flex gap-2">
            <TriangleAlert className="mt-0.5 size-4 shrink-0" />
            Not a mission: nothing was planned.
          </p>
          <p className="font-sans text-muted-foreground">{triage.reason}</p>
          <p className="font-sans text-muted-foreground">
            The assistant only compiles a reconnaissance of an area into a plan, such as{' '}
            <span className="italic">{example}</span>.
          </p>
        </div>
      </div>
    );
  }

  return (
    <div className="space-y-4">
      <blockquote className="border-l-2 border-cyan-400/60 pl-3 font-sans text-muted-foreground italic">{outcome.intent}</blockquote>
      {plan ? (
        <PlanView plan={plan} outcome={outcome} />
      ) : (
        <p className="flex gap-2 text-amber-400">
          <TriangleAlert className="mt-0.5 size-4 shrink-0" />
          No plan: the validator refused every attempt. Nothing was relaxed to make one pass.
        </p>
      )}
      {refused.length > 0 && (
        <details open={!plan} className="space-y-2">
          <summary className="cursor-pointer text-xs text-muted-foreground">
            {refused.length} refused {refused.length === 1 ? 'attempt' : 'attempts'}
          </summary>
          {refused.map((a) => (
            <AttemptView key={a.n} attempt={a} />
          ))}
        </details>
      )}
      {(ir ?? plan) && (
        <Tabs defaultValue={ir ? 'ir' : 'expanded'}>
          <TabsList>
            {ir && <TabsTrigger value="ir">Plan IR</TabsTrigger>}
            {plan && <TabsTrigger value="expanded">Expanded</TabsTrigger>}
          </TabsList>
          {ir && (
            <TabsContent value="ir">
              <Source>{irSource(ir)}</Source>
            </TabsContent>
          )}
          {plan && (
            <TabsContent value="expanded">
              <p className="mb-1 text-xs text-muted-foreground">The raster bitmaps are elided here; the hash covers them.</p>
              <Source>{expandedSource(plan)}</Source>
            </TabsContent>
          )}
        </Tabs>
      )}
    </div>
  );
}

function PlanView({ plan, outcome }: { plan: ApprovedPlan; outcome: Outcome }) {
  const rows = laneRows(plan, outcome.assignments);
  const tactic = outcome.ir?.tactic;

  return (
    <div className="space-y-4">
      <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 font-mono text-xs">
        <Fact term="Area">{plan.area.name}</Fact>
        <Fact term="Doctrine">
          {plan.doctrine.name}@{plan.doctrine.version} <span className="text-muted-foreground">{shortHash(plan.doctrine_hash)}</span>
        </Fact>
        {tactic && (
          <Fact term="Tactic">
            {[tactic.pattern, tactic.orientation, tactic.overlap_pct !== undefined ? `${tactic.overlap_pct}% overlap` : undefined]
              .filter(Boolean)
              .join(', ')}
          </Fact>
        )}
        <Fact term="Swath">
          {plan.swath_m} m at {plan.scan_alt_m} m
        </Fact>
        <Fact term="Policy">{plan.policy}</Fact>
        {plan.requires.length > 0 && <Fact term="Requires">{plan.requires.join(', ')}</Fact>}
        {plan.gcs?.length ? <Fact term="GCS">{plan.gcs.map((s) => s.name).join(', ')}</Fact> : null}
        <Fact term="Plan">
          <span title={plan.hash}>{shortHash(plan.hash)}</span>
        </Fact>
      </dl>

      <section className="space-y-1">
        <h3 className="font-mono text-xs tracking-widest text-muted-foreground uppercase">Rationale</h3>
        <p className="font-sans leading-relaxed whitespace-pre-wrap">{plan.rationale}</p>
      </section>

      <section className="space-y-1">
        <h3 className="font-mono text-xs tracking-widest text-muted-foreground uppercase">
          {rows.length} {rows.length === 1 ? 'lane' : 'lanes'}
        </h3>
        <table className="w-full font-mono text-xs">
          <thead className="text-left text-muted-foreground">
            <tr>
              <th className="py-1 font-normal">Lane</th>
              <th className="py-1 text-right font-normal">Steps</th>
              <th className="py-1 text-right font-normal">Cells</th>
              <th className="py-1 pl-3 font-normal">Vector</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r.id} title={r.rationale} className="border-t border-border/50">
                <td className="py-1">{r.id}</td>
                <td className="py-1 text-right">{r.steps}</td>
                <td className="py-1 text-right">{r.cells}</td>
                <td className={r.vector ? 'py-1 pl-3' : 'py-1 pl-3 text-amber-400'}>{r.vector ?? 'none'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </section>
    </div>
  );
}

function AttemptView({ attempt }: { attempt: Attempt }) {
  return (
    <div className="space-y-1 rounded-md border p-2 text-xs">
      <p className="font-mono text-muted-foreground">Attempt {attempt.n}</p>
      <ul className="space-y-1">
        {attempt.diagnostics?.map((d, i) => (
          <li key={i}>
            <span className="font-mono text-amber-400">
              {d.gate}/{d.code}
            </span>{' '}
            {d.message}
            {d.pointer && <span className="font-mono text-muted-foreground"> at {d.pointer}</span>}
            {d.alternatives?.length ? <span className="text-muted-foreground"> (known: {d.alternatives.join(', ')})</span> : null}
          </li>
        ))}
      </ul>
    </div>
  );
}

function Fact({ term, children }: { term: string; children: ReactNode }) {
  return (
    <>
      <dt className="text-muted-foreground">{term}</dt>
      <dd className="min-w-0 break-words">{children}</dd>
    </>
  );
}

function Source({ children }: { children: string }) {
  return <pre className="max-h-80 overflow-auto rounded-md bg-background p-2 font-mono text-[11px] leading-snug">{children}</pre>;
}
