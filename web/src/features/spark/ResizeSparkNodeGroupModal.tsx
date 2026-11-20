import { useState } from 'react'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import Button from '@cloudscape-design/components/button'
import FormField from '@cloudscape-design/components/form-field'
import Input from '@cloudscape-design/components/input'
import Modal from '@cloudscape-design/components/modal'
import SpaceBetween from '@cloudscape-design/components/space-between'

import { useAppDispatch } from '@/app/store'
import {
  useScaleSparkNodeGroupMutation,
  type SparkCluster,
  type SparkNodeGroup,
} from '@/features/spark/api'
import { MAX_WORKERS, MIN_WORKERS, workerCountProblem } from '@/features/spark/state'
import { notify } from '@/features/notifications/notificationsSlice'
import { errorMessage, fieldErrors, type ApiError } from '@/lib/apiError'

interface ResizeSparkNodeGroupModalProps {
  cluster: SparkCluster
  nodeGroup: SparkNodeGroup
  onClose: () => void
}

/**
 * ResizeSparkNodeGroupModal sets a node group's size (09-spark.md section 4).
 * Going down decommissions the newest workers first through their shutdown;
 * the dialog says so, and that one which does not finish in time is forced
 * off and leaves the cluster degraded.
 */
export function ResizeSparkNodeGroupModal({
  cluster,
  nodeGroup,
  onClose,
}: ResizeSparkNodeGroupModalProps) {
  const dispatch = useAppDispatch()
  const [scale, { isLoading }] = useScaleSparkNodeGroupMutation()
  const [size, setSize] = useState(String(nodeGroup.desired_size))
  const [localError, setLocalError] = useState<string | null>(null)
  const [error, setError] = useState<ApiError>(undefined)

  const desired = Number(size)
  const shrinking = Number.isInteger(desired) && desired < nodeGroup.desired_size

  async function confirm() {
    setError(undefined)
    const problem = workerCountProblem(desired)
    setLocalError(problem)
    if (problem !== null) {
      return
    }
    try {
      await scale({
        clusterId: cluster.id,
        nodeGroupId: nodeGroup.id,
        desiredSize: desired,
      }).unwrap()
      dispatch(
        notify({
          type: 'in-progress',
          header: 'Node group resizing',
          content: `${cluster.name}: ${nodeGroup.name} from ${nodeGroup.desired_size} to ${desired} nodes.`,
        }),
      )
      onClose()
    } catch (err) {
      setError(err as ApiError)
    }
  }

  return (
    <Modal
      visible
      onDismiss={onClose}
      header={`Resize ${nodeGroup.name}`}
      closeAriaLabel="Close"
      footer={
        <Box float="right">
          <SpaceBetween direction="horizontal" size="xs">
            <Button variant="link" onClick={onClose}>
              Cancel
            </Button>
            <Button
              variant="primary"
              loading={isLoading}
              disabled={desired === nodeGroup.desired_size}
              onClick={() => void confirm()}
              data-testid="confirm-resize"
            >
              Resize
            </Button>
          </SpaceBetween>
        </Box>
      }
    >
      <SpaceBetween size="m">
        <FormField
          label="Nodes"
          constraintText={`${MIN_WORKERS}-${MAX_WORKERS} workers of ${nodeGroup.instance_type}.`}
          errorText={localError ?? fieldErrors(error)['desired_size']}
        >
          <Input
            type="number"
            value={size}
            onChange={({ detail }) => setSize(detail.value)}
            ariaLabel="Nodes"
          />
        </FormField>
        {shrinking && (
          <Alert type="info" header="The newest workers are decommissioned first">
            Each one is shut down gracefully: Spark moves its cached and shuffle data to the other
            workers and lets its running tasks finish. A worker that is not done within the drain
            timeout (10 minutes by default) is powered off anyway, its work re-runs elsewhere, and
            the cluster shows as degraded naming it.
          </Alert>
        )}
        {error !== undefined && Object.keys(fieldErrors(error)).length === 0 && (
          <Alert type="error" header="The node group was not resized">
            {errorMessage(error, 'The node group could not be resized.')}
          </Alert>
        )}
      </SpaceBetween>
    </Modal>
  )
}
