import { useState } from 'react'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import Button from '@cloudscape-design/components/button'
import FormField from '@cloudscape-design/components/form-field'
import Input from '@cloudscape-design/components/input'
import Modal from '@cloudscape-design/components/modal'
import SpaceBetween from '@cloudscape-design/components/space-between'

import { useAppDispatch } from '@/app/store'
import { useDeleteClusterMutation, type Cluster } from '@/features/kubernetes/api'
import { notify } from '@/features/notifications/notificationsSlice'
import { errorMessage, type ApiError } from '@/lib/apiError'

interface DeleteClusterModalProps {
  cluster: Cluster
  onClose: () => void
  onDeleted?: () => void
}

/**
 * DeleteClusterModal confirms deleting a cluster. Every node goes with it, and
 * so does everything on them: workloads, and `local-path` volumes, which live
 * on the nodes' own disks (08-kubernetes.md §4). The name is typed to confirm.
 */
export function DeleteClusterModal({ cluster, onClose, onDeleted }: DeleteClusterModalProps) {
  const dispatch = useAppDispatch()
  const [deleteCluster, { isLoading }] = useDeleteClusterMutation()
  const [typed, setTyped] = useState('')
  const [error, setError] = useState<ApiError>(undefined)

  async function confirm() {
    setError(undefined)
    try {
      await deleteCluster(cluster.id).unwrap()
      dispatch(
        notify({
          type: 'in-progress',
          header: 'Cluster deletion started',
          content: `${cluster.name} and its ${cluster.nodes.length} nodes are being deleted by a background task.`,
        }),
      )
      onDeleted?.()
      onClose()
    } catch (err) {
      setError(err as ApiError)
    }
  }

  return (
    <Modal
      visible
      onDismiss={onClose}
      header={`Delete ${cluster.name}?`}
      closeAriaLabel="Close"
      footer={
        <Box float="right">
          <SpaceBetween direction="horizontal" size="xs">
            <Button variant="link" onClick={onClose}>
              Cancel
            </Button>
            <Button
              variant="primary"
              disabled={typed !== cluster.name}
              loading={isLoading}
              onClick={() => void confirm()}
              data-testid="confirm-delete-cluster"
            >
              Delete cluster
            </Button>
          </SpaceBetween>
        </Box>
      }
    >
      <SpaceBetween size="m">
        <Alert type="warning" header="Everything on the cluster is deleted with it">
          All {cluster.nodes.length} nodes are deleted, with every workload running on them. Volumes
          on the default <Box variant="code">local-path</Box> storage class live on the nodes&apos;
          disks and are deleted too. There is no way to undo this.
        </Alert>
        <FormField
          label="To confirm, type the cluster name"
          description="The cluster and its nodes are permanently deleted."
        >
          <Input
            value={typed}
            onChange={({ detail }) => setTyped(detail.value)}
            ariaLabel="Cluster name to confirm"
            placeholder={cluster.name}
          />
        </FormField>
        {error !== undefined && (
          <Alert type="error" header="The cluster was not deleted">
            {errorMessage(error, 'The cluster could not be deleted.')}
          </Alert>
        )}
      </SpaceBetween>
    </Modal>
  )
}
