import { type Fault, type FaultKind, type KeelClient, type VectorID } from '@keel/sdk';
import { useMutation } from '@tanstack/react-query';
import { Check, LoaderCircle, Zap } from 'lucide-react';
import { type FormEvent, useId, useState, useSyncExternalStore } from 'react';

import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { api } from '@/lib/api';
import { FAULT_KINDS, faultKind, faultRequest } from '@/lib/faultForm';
import { type MissionSource } from '@/lib/missionStore';
import { cn } from '@/lib/utils';

export interface FaultPanelProps {
  store: MissionSource;
  /** The screen's selected vector, the one a fault is about. */
  selected: VectorID | undefined;
  /** Selects a vector, as the roster does. */
  onSelect: (id: VectorID) => void;
  client?: KeelClient;
}

/**
 * The *Faults* tab of the right column (ControlColumn.tsx): the actions sit
 * on the right, the fleet and its decisions on the left (design section
 * 8.3). A fault is always about one vector, picked here among the fleet or in
 * the roster, the screen holding one selection; picking one here selects it
 * everywhere, the globe flying to it and the trace keeping to it.
 */
export function FaultPanel({ store, selected, onSelect, client = api }: FaultPanelProps) {
  // A string, compared by value: a tick that changes no vector re-renders nothing here.
  const known = useSyncExternalStore(store.subscribe, () => store.get()?.vectors.map((v) => v.caps.id).join(' ') ?? '');
  const vectors = known.split(' ').filter(Boolean);

  return (
    <div className="space-y-3 p-4 text-xs">
      <p className="font-sans text-muted-foreground">
        Faults are simulated: the simulator serving the vector undergoes them, and the engine sees only their consequences.
      </p>
      {vectors.length === 0 ? (
        <p className="text-muted-foreground">No vector has joined.</p>
      ) : (
        <div role="radiogroup" aria-label="Vector" className="flex flex-wrap gap-1">
          {vectors.map((id) => (
            <button
              key={id}
              type="button"
              role="radio"
              aria-checked={id === selected}
              onClick={() => onSelect(id)}
              className={cn('rounded-md border px-2 py-1 font-mono hover:bg-muted/50', id === selected && 'border-amber-400/60 bg-muted text-foreground')}
            >
              {id}
            </button>
          ))}
        </div>
      )}
      {selected ? (
        <FaultForm key={selected} vector={selected} client={client} />
      ) : (
        vectors.length > 0 && <p className="text-muted-foreground">Pick the vector to fault, here or in the roster.</p>
      )}
    </div>
  );
}

/**
 * Injects a fault into one vector (spec section 8.1), the controls the
 * reference demo loses DRONE-02 with. keeld hands the fault to the simulator
 * serving the vector and records it once the simulator accepted it, so a
 * vector no simulator serves is refused with keeld's reason. Accepted, not
 * applied: the fault lands at the next tick, and the engine sees only its
 * consequences (a silence, a drained battery), as it would on a real vehicle;
 * the decisions they cause show in the trace.
 */
function FaultForm({ vector, client }: { vector: VectorID; client: KeelClient }) {
  const [kind, setKind] = useState<FaultKind>('kill');
  const [magnitude, setMagnitude] = useState('');
  const [durationS, setDurationS] = useState('');
  const [invalid, setInvalid] = useState<string | undefined>(undefined);
  const inject = useMutation({ mutationFn: (fault: Fault) => client.injectFault(fault) });
  const spec = faultKind(kind);
  const ids = { magnitude: useId(), duration: useId() };

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const request = faultRequest({ vector, kind, magnitude, durationS });
    if ('error' in request) {
      setInvalid(request.error);
      return;
    }
    setInvalid(undefined);
    inject.mutate(request.fault);
  };

  return (
    <form aria-label={`Faults for ${vector}`} onSubmit={submit} className="space-y-2 border-t pt-3">
      <p className="font-mono tracking-widest text-muted-foreground uppercase">
        Fault <span className="text-foreground normal-case">{vector}</span>
      </p>
      <div role="radiogroup" aria-label="Kind" className="flex flex-wrap gap-1">
        {FAULT_KINDS.map((k) => (
          <button
            key={k.kind}
            type="button"
            role="radio"
            aria-checked={k.kind === kind}
            onClick={() => {
              setKind(k.kind);
              inject.reset();
            }}
            className={cn(
              'rounded-md border px-2 py-1 font-mono hover:bg-muted/50',
              k.kind === kind && 'border-amber-400/60 bg-muted text-foreground',
            )}
          >
            {k.label}
          </button>
        ))}
      </div>
      <p className="text-muted-foreground">{spec.effect}</p>
      {(spec.magnitude || spec.duration) && (
        <div className="flex gap-2">
          {spec.magnitude && (
            <label htmlFor={ids.magnitude} className="flex-1 space-y-1">
              <span className="text-muted-foreground">Magnitude, {spec.magnitude}</span>
              <Input id={ids.magnitude} inputMode="decimal" value={magnitude} onChange={(e) => setMagnitude(e.target.value)} className="font-mono" />
            </label>
          )}
          {spec.duration && (
            <label htmlFor={ids.duration} className="flex-1 space-y-1">
              <span className="text-muted-foreground">Duration, s (0: rest of run)</span>
              <Input id={ids.duration} inputMode="decimal" value={durationS} onChange={(e) => setDurationS(e.target.value)} placeholder="0" className="font-mono" />
            </label>
          )}
        </div>
      )}
      <Button type="submit" variant="destructive" size="sm" className="w-full" disabled={inject.isPending}>
        {inject.isPending ? <LoaderCircle className="animate-spin" /> : <Zap />} Inject {spec.label.toLowerCase()}
      </Button>
      {invalid && <p className="text-destructive">{invalid}</p>}
      {inject.error && (
        <p role="alert" className="text-destructive">
          {inject.error.message}
        </p>
      )}
      {inject.isSuccess && (
        <p className="flex gap-2">
          <Check className="size-4 shrink-0 text-emerald-400" /> Accepted: injected at the next tick.
        </p>
      )}
    </form>
  );
}
