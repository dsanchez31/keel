import { type Approval, KeelApiError, type KeelClient, type Outcome, type PlanHash } from '@keel/sdk';
import { useMutation } from '@tanstack/react-query';

import { api } from './api';

/** Which request is in flight. */
export type GateBusy = 'compile' | 'approve' | 'discard' | undefined;

/**
 * The human gate as the screen drives it (spec section 8.1): intent
 * compiled into an outcome, then its plan approved or discarded.
 */
export interface PlanGate {
  /**
   * The last compilation's outcome, a plan or every attempt refused, until
   * its plan is approved or discarded. A compilation the backend failed (502)
   * still shows the attempts made before it.
   */
  outcome: Outcome | undefined;
  /** The approval keeld accepted, until the next compilation. */
  approval: Approval | undefined;
  /** The last request's failure, if it failed. */
  error: Error | null;
  busy: GateBusy;
  compile(intent: string): void;
  /** Approves the outcome's plan; nothing without one. */
  approve(): void;
  /** Discards the outcome's plan; nothing without one. */
  discard(): void;
}

/**
 * One compilation at a time, held until the operator decides. Approval and
 * discard clear the outcome: the pending plan is gone from keeld then, and
 * the stream shows what the engine made of an approval (a mission started, or
 * a refusal as a decision). A new compilation clears the last approval.
 */
export function usePlanGate(client: KeelClient = api): PlanGate {
  const compile = useMutation({ mutationFn: (intent: string) => client.compile({ intent }) });
  const approve = useMutation({ mutationFn: (plan: PlanHash) => client.approve(plan), onSuccess: () => compile.reset() });
  const discard = useMutation({ mutationFn: (plan: PlanHash) => client.discard(plan), onSuccess: () => compile.reset() });

  const failed = compile.error instanceof KeelApiError ? compile.error.problem?.outcome : undefined;
  const outcome = compile.data ?? failed;
  const plan = outcome?.plan;

  return {
    outcome,
    approval: approve.data,
    error: compile.error ?? approve.error ?? discard.error,
    busy: compile.isPending ? 'compile' : approve.isPending ? 'approve' : discard.isPending ? 'discard' : undefined,
    compile(intent) {
      approve.reset();
      discard.reset();
      compile.mutate(intent);
    },
    approve() {
      if (plan) approve.mutate(plan.hash);
    },
    discard() {
      if (plan) discard.mutate(plan.hash);
    },
  };
}
