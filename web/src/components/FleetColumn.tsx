import { type FleetVectorView, type VectorID } from '@keel/sdk';
import { type LayoutStorage, useDefaultLayout } from 'react-resizable-panels';

import { ResizableHandle, ResizablePanel, ResizablePanelGroup } from '@/components/ui/resizable';
import { type MissionSource } from '@/lib/missionStore';

import { DecisionTrace } from './DecisionTrace';
import { VectorRoster } from './VectorRoster';

export interface FleetColumnProps {
  store: MissionSource;
  selected: VectorID | undefined;
  /** Selects a vector, or clears the selection with undefined. */
  onSelect: (id: VectorID | undefined) => void;
  /** The live fleet's health, for the vectors not joined yet; a replay passes none. */
  fleet?: readonly FleetVectorView[];
}

/**
 * The left column, what the operator watches and nothing to act on (design
 * section 8.3): the vector roster above the decision trace, two sections
 * of their own around a separator the operator drags, or moves with the
 * arrow keys once focused, to give the trace room while a reassignment
 * unfolds. The split is remembered in this browser only.
 */
export function FleetColumn({ store, selected, onSelect, fleet }: FleetColumnProps) {
  const layout = useDefaultLayout({ id: 'keel-fleet-column', storage, onlySaveAfterUserInteractions: true });

  return (
    <ResizablePanelGroup
      orientation="vertical"
      defaultLayout={layout.defaultLayout}
      onLayoutChanged={layout.onLayoutChanged}
      className="min-h-0 border-r bg-card text-card-foreground"
    >
      <ResizablePanel id="vectors" defaultSize="40" minSize="15">
        <VectorRoster store={store} selected={selected} onSelect={onSelect} fleet={fleet} />
      </ResizablePanel>
      <ResizableHandle withHandle aria-label="Resize the roster and the decision trace" />
      <ResizablePanel id="decisions" defaultSize="60" minSize="20">
        <DecisionTrace store={store} selected={selected} onSelect={onSelect} />
      </ResizablePanel>
    </ResizablePanelGroup>
  );
}

// localStorage when the browser allows it: a private window or blocked site
// data throws, and the column then starts from its default split.
const storage: LayoutStorage = {
  getItem(key) {
    try {
      return localStorage.getItem(key);
    } catch {
      return null;
    }
  },
  setItem(key, value) {
    try {
      localStorage.setItem(key, value);
    } catch {
      // Not remembered: a convenience, not state.
    }
  },
};
