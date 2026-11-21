import { useState } from 'react'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import Button from '@cloudscape-design/components/button'
import FormField from '@cloudscape-design/components/form-field'
import Input from '@cloudscape-design/components/input'
import Modal from '@cloudscape-design/components/modal'
import SpaceBetween from '@cloudscape-design/components/space-between'

import { useAppDispatch } from '@/app/store'
import { useDeleteSparkClusterMutation, type SparkCluster } from '@/features/spark/api'
import { notify } from '@/features/notifications/notificationsSlice'
import { errorMessage, type ApiError } from '@/lib/apiError'

interface DeleteSparkClusterModalProps {
  cluster: SparkCluster
  onClose: () => void
  onDeleted?: () => void
}

/**
 * DeleteSparkClusterModal confirms deleting a Spark cluster. Every node goes
 * with it, with the jobs running on them and the cluster's own access key; the
 * buckets and the event logs in them are the user's and stay (09-spark.md
 * section 4). The name is typed to confirm.
 */
export function DeleteSparkClusterModal({
  cluster,
  onClose,
  onDeleted,
}: DeleteSparkClusterModalProps) {
  const dispatch = useAppDispatch()
  const [deleteCluster, { isLoading }] = useDeleteSparkClusterMutation()
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
              data-testid="confirm-delete-spark-cluster"
            >
              Delete cluster
            </Button>
          </SpaceBetween>
        </Box>
      }
    >
      <SpaceBetween size="m">
        <Alert type="warning" header="Every node and running job is deleted with it">
          All {cluster.nodes.length} nodes are deleted, with every job running on them, and so is
          the cluster&apos;s own access key. Its buckets
          {cluster.buckets.length > 0 ? ` (${cluster.buckets.join(', ')})` : ''} and the event logs
          in them are kept. There is no way to undo this.
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
