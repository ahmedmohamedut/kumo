import type { StatusIndicatorProps } from '@cloudscape-design/components'

import type { Task } from '@/features/tasks/api'

export type TaskState = Task['state']

/**
 * State → StatusIndicator mapping for tasks, the task-shaped twin of
 * `features/instances/state.ts`.
 *
 * It lives here rather than beside the first page that needed it because three
 * pages now render a task's outcome — an image's history, a VPC's, and the
 * dashboard's recent list — and a fourth copy of the same switch would be the
 * one that drifts.
 *
 * `pending` reads as "Queued" rather than "Pending": an operator looking at a
 * task wants to know it has not started yet, and `pending` is already the word
 * an instance uses for a launch that is under way.
 */
export const TASK_INDICATOR: Record<TaskState, { type: StatusIndicatorProps.Type; label: string }> =
  {
    pending: { type: 'pending', label: 'Queued' },
    running: { type: 'in-progress', label: 'Running' },
    succeeded: { type: 'success', label: 'Succeeded' },
    failed: { type: 'error', label: 'Failed' },
  }

/** taskIndicatorType maps a task state onto a StatusIndicator type. */
export function taskIndicatorType(state: TaskState): StatusIndicatorProps.Type {
  return TASK_INDICATOR[state].type
}

/** taskLabel is the human name of a task state. */
export function taskLabel(state: TaskState): string {
  return TASK_INDICATOR[state].label
}
