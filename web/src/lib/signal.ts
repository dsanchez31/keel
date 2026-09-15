import { useSyncExternalStore } from 'react';

/**
 * A value that changes on every frame Cesium draws (a replay's playhead, the
 * position under the cursor), held outside React so a change re-renders only
 * what reads it, not the screen around it.
 */
export interface Signal<T> {
  get(): T;
  set(value: T): void;
  subscribe(listener: () => void): () => void;
}

export function createSignal<T>(initial: T): Signal<T> {
  let value = initial;
  const listeners = new Set<() => void>();
  return {
    get: () => value,
    set(next) {
      if (Object.is(next, value)) return;
      value = next;
      for (const listener of [...listeners]) listener();
    },
    subscribe(listener) {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    },
  };
}

/** The signal's value for a component, re-rendering it on every change. */
export function useSignal<T>(signal: Signal<T>): T {
  return useSyncExternalStore(signal.subscribe, signal.get);
}
