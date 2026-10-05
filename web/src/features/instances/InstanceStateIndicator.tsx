import Box from '@cloudscape-design/components/box'
import SpaceBetween from '@cloudscape-design/components/space-between'
import StatusIndicator from '@cloudscape-design/components/status-indicator'

import { STATE_INDICATOR, stateReasonText, type Instance } from '@/features/instances/state'

/**
 * InstanceStateIndicator renders one instance's state and the reason behind it.
 *
 * The reason is the whole point. `state_reason` is where the node says *why* an
 * instance went to `error` — a missing image, a root disk too small to boot, a
 * libvirt refusal — and it is the only place that information exists. A table
 * that says only "Error" sends the operator to the node to guess.
 *
 * It is rendered as inactive small text under the indicator rather than as the
 * indicator's own tooltip: a tooltip is only reachable on hover, so it hides the
 * one thing an operator scanning a column of failures is looking for.
 */
export function InstanceStateIndicator({ instance }: { instance: Instance }) {
  const indicator = STATE_INDICATOR[instance.state]
  const reason = stateReasonText(instance)

  return (
    <SpaceBetween size="xs">
      <StatusIndicator type={indicator.type}>{indicator.label}</StatusIndicator>

      {reason !== null && (
        <Box variant="small" color="text-status-inactive">
          {reason}
        </Box>
      )}
    </SpaceBetween>
  )
}
