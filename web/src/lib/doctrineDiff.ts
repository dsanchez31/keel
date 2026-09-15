import { type ConstraintDoc, type Document, type RoleDoc, type RuleDoc } from '@keel/sdk';

/** One entry of a diff, by id: in the second pack only, in the first only, or in both and different. */
export type Change<T> =
  | { readonly id: string; readonly change: 'added'; readonly to: T }
  | { readonly id: string; readonly change: 'removed'; readonly from: T }
  | { readonly id: string; readonly change: 'changed'; readonly from: T; readonly to: T };

/**
 * What a hot swap from one pack to another changes, section by section, each
 * sorted by id. Entries the two packs share unchanged are left out.
 *
 * Matched by id because the engine matches by id: a rule of the same id in
 * the new pack keeps its duration windows through the swap, changed or not,
 * and a rule the new pack lacks has them discarded (spec section 6.6).
 */
export interface DoctrineDiff {
  params: Change<number>[];
  roles: Change<RoleDoc>[];
  constraints: Change<ConstraintDoc>[];
  rules: Change<RuleDoc>[];
}

export function diffPacks(from: Document, to: Document): DoctrineDiff {
  const params = (doc: Document) => Object.entries(doc.params).map(([id, value]) => ({ id, value: value as number }));
  return {
    params: diffById(params(from), params(to), (p) => p.id, (a, b) => a.value === b.value).map(unwrap),
    roles: diffById(from.roles ?? [], to.roles ?? [], (r) => r.id, (a, b) => sameList(a.requires, b.requires)),
    constraints: diffById(from.constraints ?? [], to.constraints ?? [], (c) => c.id, (a, b) => a.rule === b.rule),
    rules: diffById(from.rules ?? [], to.rules ?? [], (r) => r.id, (a, b) => a.when === b.when && a.then === b.then && a.priority === b.priority),
  };
}

/** Whether a diff changes nothing. */
export const isEmpty = (d: DoctrineDiff): boolean =>
  d.params.length + d.roles.length + d.constraints.length + d.rules.length === 0;

function diffById<T>(from: readonly T[], to: readonly T[], id: (t: T) => string, same: (a: T, b: T) => boolean): Change<T>[] {
  const before = new Map(from.map((t) => [id(t), t]));
  const after = new Map(to.map((t) => [id(t), t]));
  const ids = [...new Set([...before.keys(), ...after.keys()])].sort();
  const out: Change<T>[] = [];
  for (const key of ids) {
    const a = before.get(key);
    const b = after.get(key);
    if (a === undefined && b !== undefined) out.push({ id: key, change: 'added', to: b });
    else if (a !== undefined && b === undefined) out.push({ id: key, change: 'removed', from: a });
    else if (a !== undefined && b !== undefined && !same(a, b)) out.push({ id: key, change: 'changed', from: a, to: b });
  }
  return out;
}

function unwrap(c: Change<{ id: string; value: number }>): Change<number> {
  switch (c.change) {
    case 'added':
      return { id: c.id, change: 'added', to: c.to.value };
    case 'removed':
      return { id: c.id, change: 'removed', from: c.from.value };
    case 'changed':
      return { id: c.id, change: 'changed', from: c.from.value, to: c.to.value };
  }
}

function sameList(a: readonly string[], b: readonly string[]): boolean {
  return a.length === b.length && a.every((x, i) => x === b[i]);
}
