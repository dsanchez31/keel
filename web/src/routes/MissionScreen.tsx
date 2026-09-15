import { type AreaView, type VectorID } from '@keel/sdk';
import { useState } from 'react';

import { flyOver } from '@/cesium/camera';
import { CesiumViewer } from '@/cesium/CesiumViewer';
import { CursorTracker } from '@/cesium/CursorTracker';
import { FogOfWar } from '@/cesium/FogOfWar';
import { MissionGlobe } from '@/cesium/MissionGlobe';
import { PlanPreview } from '@/cesium/PlanPreview';
import { getViewer } from '@/cesium/viewerManager';
import { WorldAreas } from '@/cesium/WorldAreas';
import { ControlColumn } from '@/components/ControlColumn';
import { FleetColumn } from '@/components/FleetColumn';
import { LiveTimeControl } from '@/components/TimeControl';
import { type GroundPoint } from '@/lib/geoFormat';
import { createMissionStore } from '@/lib/missionStore';
import { createSignal } from '@/lib/signal';
import { useFleet } from '@/lib/useFleet';
import { useLiveClock } from '@/lib/useLiveClock';
import { useMissionStream } from '@/lib/useMissionStream';
import { usePlanGate } from '@/lib/usePlanGate';
import { useWorld } from '@/lib/useWorld';

/**
 * The live mission, three columns with the globe in the middle, never
 * covered: on the left what the operator watches, the vector roster and the
 * decision trace; on the right what they act with, the assistant, the
 * doctrine panel and the fault injection (design section 8.3). keeld's
 * stream is folded into one store every part reads; the globe draws it under
 * its fog of war, and the plan awaiting the human gate over it until approved
 * or discarded, with the time strip under it, where the pace of a simulated
 * fleet is set. One vector is selected at a time, in the roster, the trace or
 * the *Faults* tab: the globe flies to it, the trace keeps to its decisions,
 * and a fault is about it.
 */
export function MissionScreen() {
  const [store] = useState(createMissionStore);
  const status = useMissionStream(store.apply);
  const gate = usePlanGate();
  const clock = useLiveClock();
  const world = useWorld();
  const fleet = useFleet(store);
  const [selected, setSelected] = useState<VectorID | undefined>(undefined);
  const [cursor] = useState(() => createSignal<GroundPoint | undefined>(undefined));

  return (
    <main className="grid h-full grid-cols-[22rem_minmax(0,1fr)_26rem]">
      <FleetColumn store={store} selected={selected} onSelect={setSelected} fleet={fleet} />
      <div className="flex min-h-0 flex-col">
        <div className="min-h-0 flex-1">
          <CesiumViewer id="mission-globe" label="Mission globe">
            {(viewer) => (
              <>
                <CursorTracker viewer={viewer} cursor={cursor} />
                <WorldAreas viewer={viewer} store={store} world={world.view} />
                <FogOfWar viewer={viewer} store={store} />
                <MissionGlobe viewer={viewer} store={store} selected={selected} />
                <PlanPreview viewer={viewer} plan={gate.outcome?.plan} assignments={gate.outcome?.assignments} />
              </>
            )}
          </CesiumViewer>
        </div>
        <LiveTimeControl store={store} status={status} cursor={cursor} clock={clock} />
      </div>
      <ControlColumn store={store} gate={gate} selected={selected} onSelect={setSelected} world={world} onArea={showArea} />
    </main>
  );
}

// An area picked in the assistant brings the camera over it, as a mission's
// AO does when it appears.
function showArea(area: AreaView): void {
  const viewer = getViewer('mission-globe');
  if (viewer) flyOver(viewer, area.polygon.ring);
}
