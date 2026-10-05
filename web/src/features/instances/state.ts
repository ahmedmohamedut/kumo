import type { StatusIndicatorProps } from '@cloudscape-design/components'
import type { components } from '@/types/api'

export type Instance = components['schemas']['Instance']
export type InstanceState = components['schemas']['InstanceState']

/**
 * State → StatusIndicator mapping for instances (03-frontend.md §6.1).
 *
 * The set reads as a life cycle: arriving, up, going down, down, being removed,
 * gone, broken. `terminated` is `stopped` rather than an error on purpose — a
 * terminated instance is the outcome the operator asked for, and colouring it
 * like a failure would train them to ignore red in this table.
 */
export const STATE_INDICATOR: Record<
  InstanceState,
  { type: StatusIndicatorProps.Type; label: string }
> = {
  pending: { type: 'pending', label: 'Pending' },
  running: { type: 'success', label: 'Running' },
  stopping: { type: 'in-progress', label: 'Stopping' },
  stopped: { type: 'stopped', label: 'Stopped' },
  terminating: { type: 'in-progress', label: 'Terminating' },
  terminated: { type: 'stopped', label: 'Terminated' },
  error: { type: 'error', label: 'Error' },
}

/** stateLabel is the human name of an instance state. */
export function stateLabel(state: InstanceState): string {
  return STATE_INDICATOR[state].label
}

/**
 * stateReasonText explains an instance state, or null when there is nothing to add.
 *
 * The server's `state_reason` is the only thing that tells an operator whether a
 * failed boot was a missing image, a too-small disk or a libvirt error, so it is
 * never dropped on the floor. A pending instance says what it is waiting for
 * instead, because "Pending" alone does not say whether anything is happening.
 */
export function stateReasonText(instance: Instance): string | null {
  if (instance.state === 'error') {
    return instance.state_reason ?? 'The instance is in an error state without a reason.'
  }
  if (instance.state === 'pending') {
    return instance.state_reason ?? 'Waiting for the node to build the instance.'
  }
  return null
}
