import { useState } from 'react'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import Button from '@cloudscape-design/components/button'
import FormField from '@cloudscape-design/components/form-field'
import Input from '@cloudscape-design/components/input'
import Modal from '@cloudscape-design/components/modal'
import SpaceBetween from '@cloudscape-design/components/space-between'

import { useAppDispatch } from '@/app/store'
import { useDeleteDBInstanceMutation, type DBInstance } from '@/features/dbinstances/api'
import { notify } from '@/features/notifications/notificationsSlice'
import { errorMessage, type ApiError } from '@/lib/apiError'

interface DeleteDBInstanceModalProps {
  dbInstance: DBInstance
  onClose: () => void
  /** onDeleted runs once the delete has been queued. */
  onDeleted?: () => void
}

/**
 * DeleteDBInstanceModal confirms deleting a database.
 *
 * There is no final snapshot and no recovery: the data disk goes with the
 * server (07-databases.md §4). So the dialog says that in as many words and
 * asks for the identifier to be typed, the way the terminate dialog asks for
 * an instance name.
 */
export function DeleteDBInstanceModal({
  dbInstance,
  onClose,
  onDeleted,
}: DeleteDBInstanceModalProps) {
  const dispatch = useAppDispatch()
  const [deleteDBInstance, { isLoading }] = useDeleteDBInstanceMutation()
  const [typed, setTyped] = useState('')
  const [error, setError] = useState<ApiError>(undefined)

  const confirmed = typed === dbInstance.identifier

  async function confirm() {
    setError(undefined)
    try {
      await deleteDBInstance(dbInstance.id).unwrap()
      dispatch(
        notify({
          type: 'in-progress',
          header: 'Database deletion started',
          content: `${dbInstance.identifier} and its storage are being deleted by a background task.`,
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
      header={`Delete ${dbInstance.identifier}?`}
      closeAriaLabel="Close"
      footer={
        <Box float="right">
          <SpaceBetween direction="horizontal" size="xs">
            <Button variant="link" onClick={onClose}>
              Cancel
            </Button>
            <Button
              variant="primary"
              disabled={!confirmed}
              loading={isLoading}
              onClick={() => void confirm()}
              data-testid="confirm-delete-db-instance"
            >
              Delete database
            </Button>
          </SpaceBetween>
        </Box>
      }
    >
      <SpaceBetween size="m">
        <Alert type="warning" header="The data is deleted with the database">
          The {dbInstance.storage_gb} GiB of storage holding {dbInstance.database_name} is deleted
          with the server. There is no snapshot and no way to undo this.
        </Alert>

        <Box variant="p">
          Applications connecting to{' '}
          <Box variant="code">{dbInstance.endpoint ?? 'this database'}</Box> will stop being able to
          reach it.
        </Box>

        <FormField
          label="To confirm, type the database identifier"
          description="Everything in the database is permanently deleted."
        >
          <Input
            value={typed}
            onChange={({ detail }) => setTyped(detail.value)}
            ariaLabel="Database identifier to confirm"
            placeholder={dbInstance.identifier}
          />
        </FormField>

        {error !== undefined && (
          <Alert type="error" header="The database was not deleted">
            {errorMessage(error, 'The database could not be deleted.')}
          </Alert>
        )}
      </SpaceBetween>
    </Modal>
  )
}
