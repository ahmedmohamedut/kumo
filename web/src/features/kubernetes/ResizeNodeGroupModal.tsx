import { useState } from 'react'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import Button from '@cloudscape-design/components/button'
import FormField from '@cloudscape-design/components/form-field'
import Input from '@cloudscape-design/components/input'
import Modal from '@cloudscape-design/components/modal'
import SpaceBetween from '@cloudscape-design/components/space-between'

import { useAppDispatch } from '@/app/store'
import { useScaleNodeGroupMutation, type Cluster, type NodeGroup } from '@/features/kubernetes/api'
import { MAX_NODE_COUNT, MIN_NODE_COUNT, nodeCountProblem } from '@/features/kubernetes/state'
import { notify } from '@/features/notifications/notificationsSlice'
import { errorMessage, fieldErrors, type ApiError } from '@/lib/apiError'

interface ResizeNodeGroupModalProps {
  cluster: Cluster
  nodeGroup: NodeGroup
  onClose: () => void
}

/**
 * ResizeNodeGroupModal sets a node group's size (08-kubernetes.md §4). Going
 * down drains the newest workers first, and a PodDisruptionBudget can stop it:
 * the dialog says so, because the result is then `degraded` rather than the
 * size asked for.
 */
export function ResizeNodeGroupModal({ cluster, nodeGroup, onClose }: ResizeNodeGroupModalProps) {
  const dispatch = useAppDispatch()
  const [scale, { isLoading }] = useScaleNodeGroupMutation()
  const [size, setSize] = useState(String(nodeGroup.desired_size))
  const [localError, setLocalError] = useState<string | null>(null)
  const [error, setError] = useState<ApiError>(undefined)

  const desired = Number(size)
  const shrinking = Number.isInteger(desired) && desired < nodeGroup.desired_size

  async function confirm() {
    setError(undefined)
    const problem = nodeCountProblem(desired)
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
          constraintText={`${MIN_NODE_COUNT}-${MAX_NODE_COUNT} workers of ${nodeGroup.instance_type}.`}
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
          <Alert type="info" header="The newest workers are drained first">
            Their pods are evicted, which honors PodDisruptionBudgets. A budget that holds a pod for
            5 minutes stops the resize: the node is kept, and the cluster shows as degraded with the
            pod named.
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
