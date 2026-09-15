import { type ApprovedPlan, type Assignment } from '@keel/sdk';
import { type Viewer } from 'cesium';
import { useEffect } from 'react';

import { useFontReady } from '@/lib/useFontReady';

import { LABEL_FONT } from './entityStyle';
import { PlanPreviewScene } from './planPreviewScene';

export interface PlanPreviewProps {
  viewer: Viewer;
  /** The plan awaiting the gate, undefined when none does. */
  plan: ApprovedPlan | undefined;
  assignments: readonly Assignment[] | undefined;
}

/**
 * Draws the plan awaiting approval on the globe (planPreviewScene.ts) for as long
 * as it awaits: approval or discard clears it. Waits for the label font, as
 * every scene with labels does. Renders nothing of its own.
 */
export function PlanPreview({ viewer, plan, assignments }: PlanPreviewProps) {
  const fontReady = useFontReady(LABEL_FONT);

  useEffect(() => {
    if (!fontReady || !plan) return;
    const preview = new PlanPreviewScene(viewer);
    preview.show(plan, assignments);
    return () => preview.destroy();
  }, [viewer, fontReady, plan, assignments]);

  return null;
}
