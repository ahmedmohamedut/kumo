import { useState } from 'react'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import Button from '@cloudscape-design/components/button'
import Checkbox from '@cloudscape-design/components/checkbox'
import FormField from '@cloudscape-design/components/form-field'
import Input from '@cloudscape-design/components/input'
import Modal from '@cloudscape-design/components/modal'
import SpaceBetween from '@cloudscape-design/components/space-between'

import { useAppDispatch } from '@/app/store'
import { useDeleteBucketMutation } from '@/features/buckets/api'
import { notify } from '@/features/notifications/notificationsSlice'
import { envelopeOf, errorMessage, type ApiError } from '@/lib/apiError'

interface DeleteBucketModalProps {
  name: string
  onClose: () => void
  /** onDeleted runs after a delete succeeded or was queued. */
  onDeleted?: () => void
}

/**
 * DeleteBucketModal confirms a bucket delete.
 *
 * A plain delete removes an empty bucket at once. A bucket that holds objects
 * — or only old versions, which makes it look empty — is refused with `409`,
 * and the dialog then offers the forced delete: every object and version is
 * removed by a background task. Because that cannot be undone, it asks for
 * the bucket name to be typed, the way the S3 console does.
 */
export function DeleteBucketModal({ name, onClose, onDeleted }: DeleteBucketModalProps) {
  const dispatch = useAppDispatch()
  const [deleteBucket, { isLoading }] = useDeleteBucketMutation()
  const [force, setForce] = useState(false)
  const [typed, setTyped] = useState('')
  const [error, setError] = useState<ApiError>(undefined)

  const notEmpty = envelopeOf(error)?.error.code === 'conflict' && !force
  const confirmed = !force || typed === name

  async function confirm() {
    setError(undefined)
    try {
      await deleteBucket({ name, force }).unwrap()
      dispatch(
        force
          ? notify({
              type: 'in-progress',
              header: 'Bucket deletion started',
              content: `${name} is emptied of every object and version, then deleted, by a background task.`,
            })
          : notify({ type: 'success', header: 'Bucket deleted', content: name }),
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
      header={`Delete ${name}?`}
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
              data-testid="confirm-delete-bucket"
            >
              {force ? 'Delete bucket and contents' : 'Delete'}
            </Button>
          </SpaceBetween>
        </Box>
      }
    >
      <SpaceBetween size="m">
        <Box variant="p">
          Applications that use {name} will no longer be able to read or write it. This cannot be
          undone.
        </Box>

        <Checkbox
          checked={force}
          onChange={({ detail }) => {
            setForce(detail.checked)
            setTyped('')
          }}
          description="Remove every object, old version and delete marker first. Needed for a bucket that is not empty."
        >
          Delete all contents
        </Checkbox>

        {force && (
          <FormField
            label={`To confirm, type the bucket name`}
            description="Everything in the bucket is permanently deleted."
          >
            <Input
              value={typed}
              onChange={({ detail }) => setTyped(detail.value)}
              ariaLabel="Bucket name to confirm"
              placeholder={name}
            />
          </FormField>
        )}

        {notEmpty ? (
          <Alert type="warning" header="This bucket is not empty">
            It still holds objects, or old versions of deleted ones. Select{' '}
            <strong>Delete all contents</strong> to remove them and the bucket.
          </Alert>
        ) : (
          error !== undefined && (
            <Alert type="error" header="The bucket was not deleted">
              {errorMessage(error, 'The bucket could not be deleted.')}
            </Alert>
          )
        )}
      </SpaceBetween>
    </Modal>
  )
}
