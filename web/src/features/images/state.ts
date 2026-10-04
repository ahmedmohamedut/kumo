import type { StatusIndicatorProps } from '@cloudscape-design/components'
import type { components } from '@/types/api'

export type Image = components['schemas']['Image']
export type ImageState = components['schemas']['ImageState']

/**
 * State → StatusIndicator mapping for images (03-frontend.md §6.4).
 *
 * `failed` is an error and everything else is a step on the way to `available`,
 * so the mapping reads as a story: queued, moving, done, broken, going away.
 */
export const STATE_INDICATOR: Record<
  ImageState,
  { type: StatusIndicatorProps.Type; label: string }
> = {
  pending: { type: 'pending', label: 'Pending' },
  downloading: { type: 'in-progress', label: 'Downloading' },
  available: { type: 'success', label: 'Available' },
  failed: { type: 'error', label: 'Failed' },
  deleting: { type: 'in-progress', label: 'Deleting' },
}

/** stateLabel is the human name of an image state. */
export function stateLabel(state: ImageState): string {
  return STATE_INDICATOR[state].label
}

/**
 * stateReasonText explains an image state, or null when there is nothing to add.
 *
 * The server's `state_reason` is the only thing that tells an operator whether to
 * fix a URL, wait for a mirror, or ask for a bigger disk, so it is never dropped
 * on the floor: a failed import without one still says so.
 */
export function stateReasonText(image: Image): string | null {
  if (image.state === 'failed') {
    return image.state_reason ?? 'The import failed without a reason.'
  }
  if (image.state === 'pending') {
    return 'Queued for download.'
  }
  return null
}
