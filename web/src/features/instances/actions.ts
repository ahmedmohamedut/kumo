import type { InstanceState } from '@/features/instances/state'

/**
 * The lifecycle actions the console can ask for, in the order the dropdown shows
 * them. `force` marks the destructive variant of an action that has both.
 */
export type InstanceAction = 'start' | 'stop' | 'force-stop' | 'reboot' | 'terminate'

/** ACTIONS is the dropdown order, and the order the tests assert on. */
export const ACTIONS: ReadonlyArray<InstanceAction> = [
  'start',
  'stop',
  'force-stop',
  'reboot',
  'terminate',
]

/**
 * The states each action may start from.
 *
 * This is a transcription of the table in
 * `internal/service/instance/lifecycle.go` — `Start` from `stopped`, `Stop` from
 * `running` and `stopping`, `Reboot` from `running`, `Terminate` from everything a
 * live instance can be in. It is duplicated here on purpose: the API is still the
 * authority and will answer `409` regardless, but a button that is live only to
 * return a conflict is a worse experience than one that is honestly disabled, and
 * an operator who is told why is better off than one who is told no.
 *
 * Keep the two in step. A state added to one and not the other shows up as either
 * a dead button or a surprise `409`.
 */
const ALLOWED_FROM: Record<InstanceAction, ReadonlySet<InstanceState>> = {
  start: new Set<InstanceState>(['stopped']),
  // `stopping` is a source as well as a destination: a graceful stop whose guest
  // ignored the request leaves the row there, and another stop is the way out.
  stop: new Set<InstanceState>(['running', 'stopping']),
  'force-stop': new Set<InstanceState>(['running', 'stopping']),
  reboot: new Set<InstanceState>(['running']),
  // Terminate is also the way out of an instance that is stuck, so it is offered
  // from `error` and `pending` too. `terminated` is left out because such a row
  // is a leftover of the release that kept them as history: a DELETE does purge
  // one, but a button labelled Terminate on something already terminated would
  // do more than it says it does.
  terminate: new Set<InstanceState>([
    'pending',
    'running',
    'stopping',
    'stopped',
    'terminating',
    'error',
  ]),
}

/** ACTION_LABELS are the menu entries an operator reads. */
export const ACTION_LABELS: Record<InstanceAction, string> = {
  start: 'Start',
  stop: 'Stop',
  'force-stop': 'Force stop',
  reboot: 'Reboot',
  terminate: 'Terminate',
}

/**
 * ACTIONS_NEEDING_CONFIRMATION are the actions that destroy something. Force stop
 * cuts a guest's power without asking it, and terminate deletes its disks, so
 * both go through a confirmation the operator has to accept
 * (03-frontend.md §6.1). Terminate also asks for the name to be typed.
 */
export const ACTIONS_NEEDING_CONFIRMATION: ReadonlySet<InstanceAction> = new Set<InstanceAction>([
  'force-stop',
  'terminate',
])

/** canPerform answers whether `action` is legal from `state`. */
export function canPerform(action: InstanceAction, state: InstanceState): boolean {
  return ALLOWED_FROM[action].has(state)
}

/** isForce reports whether an action is its destructive variant. */
export function isForce(action: InstanceAction): boolean {
  return action === 'force-stop'
}
