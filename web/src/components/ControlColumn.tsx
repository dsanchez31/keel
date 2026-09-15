import { type AreaView, type VectorID } from '@keel/sdk';
import { useSyncExternalStore } from 'react';

import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs';
import { type MissionSource } from '@/lib/missionStore';
import { shortHash } from '@/lib/planView';
import { type PlanGate } from '@/lib/usePlanGate';
import { type World } from '@/lib/useWorld';

import { AssistantPanel } from './AssistantPanel';
import { DoctrinePanel } from './DoctrinePanel';
import { FaultPanel } from './FaultPanel';

export interface ControlColumnProps {
  store: MissionSource;
  gate: PlanGate;
  /** The screen's selected vector, the one the *Faults* tab acts on. */
  selected: VectorID | undefined;
  onSelect: (id: VectorID) => void;
  /** The world's areas and stations, listed in the assistant. */
  world: World;
  /** An area picked in the assistant: the globe shows it. */
  onArea: (area: AreaView) => void;
}

/**
 * The right column, where the operator acts (design section 8.3): the
 * assistant, the doctrine panel and the fault injection as three tabs, since
 * a hot swap or a fault happens while a mission runs and the assistant then
 * has nothing pending. The active pack, by reference and hash, stays in the
 * header whichever tab is open. The assistant stays mounted when hidden, so
 * an intent being typed survives a look at the doctrine. Selecting a vector
 * elsewhere never switches tab: the operator chooses what to do.
 */
export function ControlColumn({ store, gate, selected, onSelect, world, onArea }: ControlColumnProps) {
  // A string, compared by value: the header renders when the pack changes, not every tick.
  const pack = useSyncExternalStore(store.subscribe, () => {
    const view = store.get();
    if (!view) return '';
    const ref = view.mission.doctrine;
    return ref?.name && view.doctrine_hash ? `${ref.name}@${ref.version} ${shortHash(view.doctrine_hash)}` : '';
  });

  return (
    <Tabs defaultValue="assistant" className="flex h-full min-h-0 flex-col gap-0 border-l bg-card text-card-foreground">
      <header className="flex items-center gap-3 border-b px-4 py-2">
        <TabsList>
          <TabsTrigger value="assistant">Assistant</TabsTrigger>
          <TabsTrigger value="doctrine">Doctrine</TabsTrigger>
          <TabsTrigger value="faults">Faults</TabsTrigger>
        </TabsList>
        <span className="ml-auto truncate font-mono text-[11px] text-muted-foreground" title="Active doctrine pack">
          {pack}
        </span>
      </header>
      {/* Hidden through the `hidden` attribute, which Tailwind's preflight enforces over any display utility. */}
      <TabsContent value="assistant" keepMounted className="min-h-0 flex-1">
        <AssistantPanel gate={gate} world={world} onArea={onArea} />
      </TabsContent>
      <TabsContent value="doctrine" className="min-h-0 flex-1 overflow-y-auto">
        <DoctrinePanel store={store} />
      </TabsContent>
      <TabsContent value="faults" className="min-h-0 flex-1 overflow-y-auto">
        <FaultPanel store={store} selected={selected} onSelect={onSelect} />
      </TabsContent>
    </Tabs>
  );
}
