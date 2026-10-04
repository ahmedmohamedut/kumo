import Box from '@cloudscape-design/components/box'
import ProgressBar from '@cloudscape-design/components/progress-bar'
import SpaceBetween from '@cloudscape-design/components/space-between'
import StatusIndicator from '@cloudscape-design/components/status-indicator'

import { STATE_INDICATOR, stateReasonText, type Image } from '@/features/images/state'

/**
 * ImageStateIndicator renders one image's state, its reason and its progress.
 *
 * The progress bar appears only when the API knows a percentage, which happens
 * when the source server sent a `Content-Length`
 * (docs/design/01-api.md §Images). A bar frozen at 0% for a whole 600 MB
 * download says "nothing is happening" when something very much is, so the
 * indicator carries the state alone in that case.
 */
export function ImageStateIndicator({ image }: { image: Image }) {
  const indicator = STATE_INDICATOR[image.state]
  const reason = stateReasonText(image)
  const showProgress = image.state === 'downloading' && (image.progress ?? 0) > 0

  return (
    <SpaceBetween size="xs">
      <StatusIndicator type={indicator.type}>{indicator.label}</StatusIndicator>

      {reason !== null && (
        <Box variant="small" color="text-status-inactive">
          {reason}
        </Box>
      )}

      {showProgress && (
        <ProgressBar
          value={image.progress ?? 0}
          ariaLabel={`Download progress of ${image.name}`}
          description={`${image.progress ?? 0}%`}
        />
      )}
    </SpaceBetween>
  )
}
