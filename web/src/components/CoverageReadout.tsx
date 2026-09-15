import { useSyncExternalStore } from 'react';

import { coverageText } from '@/lib/format';
import { type MissionSource } from '@/lib/missionStore';

/**
 * How much of the AO the mission has explored, as of the last tick: the
 * share with a thin bar, and the cell count, live or replayed. Coverage is
 * the mission's goal, so it sits in the time strip, always in sight. Read as
 * a string compared by value: it renders when a tick explored a cell.
 */
export function CoverageReadout({ store }: { store: MissionSource }) {
  const counts = useSyncExternalStore(store.subscribe, () => {
    const v = store.get();
    return v && v.total > 0 ? `${v.explored} ${v.total}` : '';
  });
  if (!counts) return null;
  const [explored = 0, total = 0] = counts.split(' ').map(Number);
  const { percent, cells } = coverageText(explored, total);

  return (
    <span className="flex items-center gap-2" title={`${cells} cells of the AO explored`}>
      <span className="text-muted-foreground">coverage</span>
      <span className="h-1 w-16 overflow-hidden rounded-full bg-muted" aria-hidden>
        <span className="block h-full bg-cyan-400" style={{ width: `${(explored * 100) / total}%` }} />
      </span>
      <span className="tabular-nums">{percent}</span>
      <span className="text-muted-foreground tabular-nums">{cells}</span>
    </span>
  );
}
