import { type Viewer } from 'cesium';
import { type ReactNode, useEffect, useRef, useState } from 'react';

import { attachViewer, detachViewer, type ViewerId } from './viewerManager';

export interface CesiumViewerProps {
  /** Identifies the retained viewer. Two mounts sharing an id share a context. */
  id: ViewerId;
  /** Accessible name of the canvas region. */
  label: string;
  /**
   * What draws into the viewer or over it: entities, overlays, controls.
   * Rendered once the viewer exists, and handed it.
   */
  children?: (viewer: Viewer) => ReactNode;
}

/**
 * Attaches a retained Cesium viewer to the DOM for as long as it is mounted.
 *
 * It creates nothing and destroys nothing: viewerManager owns the WebGL
 * context, and unmounting only detaches the canvas, so a screen left and
 * revisited finds the same viewer, imagery cache and camera. What a child adds
 * to the viewer is the child's to remove on unmount.
 */
export function CesiumViewer({ id, label, children }: CesiumViewerProps) {
  const hostRef = useRef<HTMLDivElement>(null);
  // Held in state rather than read at render, so the children appear on the
  // commit that creates the viewer.
  const [viewer, setViewer] = useState<Viewer | null>(null);

  useEffect(() => {
    const host = hostRef.current;
    if (!host) return;

    const attached = attachViewer(id, host);
    setViewer(attached);

    // Cesium re-measures its canvas on window resize only. The host also
    // changes size when the layout around it does (a panel opening), which
    // fires no window event and would leave the scene stretched.
    const observer = new ResizeObserver(() => attached.resize());
    observer.observe(host);

    return () => {
      observer.disconnect();
      detachViewer(id);
    };
  }, [id]);

  return (
    <div className="relative h-full w-full bg-background">
      <div ref={hostRef} role="region" aria-label={label} className="h-full w-full" />
      {viewer && children?.(viewer)}
    </div>
  );
}
