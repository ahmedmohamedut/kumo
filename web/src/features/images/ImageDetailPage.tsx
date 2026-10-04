import { useMemo, useState, type ReactNode } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import Button from '@cloudscape-design/components/button'
import Grid from '@cloudscape-design/components/grid'
import type { GridProps } from '@cloudscape-design/components'
import Container from '@cloudscape-design/components/container'
import ContentLayout from '@cloudscape-design/components/content-layout'
import Header from '@cloudscape-design/components/header'
import Modal from '@cloudscape-design/components/modal'
import SpaceBetween from '@cloudscape-design/components/space-between'
import Spinner from '@cloudscape-design/components/spinner'
import StatusIndicator from '@cloudscape-design/components/status-indicator'

import { usePageMeta } from '@/app/pageMeta'
import { useAppDispatch, useAppSelector } from '@/app/store'
import {
  imagesApi,
  pollInterval,
  useDeleteImageMutation,
  useGetImageQuery,
  useRetryImageTaskMutation,
} from '@/features/images/api'
import { ImageStateIndicator } from '@/features/images/ImageStateIndicator'
import {
  pollInterval as taskPollInterval,
  tasksApi,
  useListTasksQuery,
  type Task,
} from '@/features/tasks/api'
import { notify } from '@/features/notifications/notificationsSlice'
import { errorMessage, type ApiError } from '@/lib/apiError'
import { formatAbsoluteTime, formatBytes, formatRelativeTime } from '@/lib/format'

/**
 * ImageDetailPage shows one image, its state and the tasks that worked on it.
 *
 * The page polls while the image is moving: 5 s while it is importing or being
 * deleted, 30 s once it has settled (03-frontend.md §5). An import of a cloud
 * image is measured in minutes, so a page that does not watch would spend most of
 * its life showing a state that happened to be true when it loaded.
 *
 * It offers exactly the two actions an image can still take — retry a failed
 * import, delete the image. An image is a downloaded file, not an object with
 * editable properties, so there is nothing else here to change.
 */
export function ImageDetailPage() {
  const { imageId } = useParams<{ imageId: string }>()
  const dispatch = useAppDispatch()
  const navigate = useNavigate()
  const [isDeleteOpen, setDeleteOpen] = useState(false)
  const [actionError, setActionError] = useState<ApiError>(undefined)

  const id = imageId ?? ''
  // Memoized so the subscription keys stay stable across renders.
  const taskArgs = useMemo(() => ({ resourceId: id }), [id])
  // The cached copies are read with selectors because a poll interval has to be
  // chosen before the subscription starts, and the data cannot come from the very
  // call that sets it up. Reading the cache fetches nothing.
  const cachedImage = useAppSelector(imagesApi.endpoints.getImage.select(id))
  const cachedTasks = useAppSelector(tasksApi.endpoints.listTasks.select(taskArgs))

  const {
    data: image,
    isLoading,
    isFetching,
    error,
  } = useGetImageQuery(id, {
    skip: id === '',
    pollingInterval: pollInterval(cachedImage?.data === undefined ? undefined : [cachedImage.data]),
  })
  const { data: tasksData } = useListTasksQuery(taskArgs, {
    skip: id === '',
    pollingInterval: taskPollInterval(cachedTasks?.data?.items),
  })

  const [deleteImage, { isLoading: isDeleting }] = useDeleteImageMutation()
  const [retryImageTask, { isLoading: isRetrying }] = useRetryImageTaskMutation()

  usePageMeta({
    breadcrumbs: [
      { text: 'Compute', href: '/' },
      { text: 'Images', href: '/images' },
      { text: image?.name ?? id, href: `/images/${id}` },
    ],
  })

  const tasks = tasksData?.items ?? []
  const tags = Object.entries(image?.tags ?? {})
  const canRetry = image?.state === 'failed'

  async function onRetry() {
    if (id === '') {
      return
    }
    setActionError(undefined)
    try {
      await retryImageTask(id).unwrap()
      dispatch(
        notify({
          type: 'in-progress',
          header: 'Retry started',
          content: 'The import is queued again, and this page follows it.',
        }),
      )
    } catch (err) {
      setActionError(err as ApiError)
    }
  }

  async function onDelete() {
    if (id === '') {
      return
    }
    setActionError(undefined)
    try {
      await deleteImage(id).unwrap()
      dispatch(
        notify({
          type: 'in-progress',
          header: 'Image deletion started',
          content: 'The downloaded copy is removed by a background task.',
        }),
      )
      navigate('/images')
    } catch (err) {
      // A 409 names the instances that still reference the image, so stay on the
      // page and show it: navigating away would throw that answer away.
      setActionError(err as ApiError)
      setDeleteOpen(false)
    }
  }

  if (id === '') {
    return null
  }

  return (
    <ContentLayout
      header={
        <SpaceBetween size="s">
          <Header
            variant="h1"
            description={image === undefined ? undefined : `Image ${image.id}`}
            actions={
              <SpaceBetween direction="horizontal" size="s">
                {canRetry && (
                  <Button
                    onClick={() => void onRetry()}
                    loading={isRetrying}
                    data-testid="retry-image"
                  >
                    Retry import
                  </Button>
                )}
                <Button onClick={() => setDeleteOpen(true)} data-testid="delete-image">
                  Delete
                </Button>
                <Button onClick={() => navigate('/images')} data-testid="back-to-images">
                  Back to images
                </Button>
              </SpaceBetween>
            }
          >
            {image?.name ?? 'Image'}
          </Header>

          {image !== undefined && (
            <SpaceBetween size="xs">
              <ImageStateIndicator image={image} />
              {isFetching && !isLoading && (
                <Box variant="small" color="text-status-inactive">
                  Updating…
                </Box>
              )}
            </SpaceBetween>
          )}
        </SpaceBetween>
      }
    >
      <SpaceBetween size="l">
        {error !== undefined && (
          <Alert type="error" header="This image could not be loaded">
            {errorMessage(error, 'This image could not be loaded.')}
          </Alert>
        )}

        {actionError !== undefined && (
          <Alert type="error" header="The request was refused">
            {errorMessage(actionError, 'The request could not be carried out.')}
          </Alert>
        )}

        {image?.state === 'deleting' && (
          <Alert type="info" header="This image is being deleted">
            The downloaded copy is removed by a background task. The image disappears from the list
            when that task finishes.
          </Alert>
        )}

        {canRetry && (
          <Alert
            type="error"
            header="The import failed"
            action={
              <Button onClick={() => void onRetry()} loading={isRetrying}>
                Retry import
              </Button>
            }
          >
            {image.state_reason ?? 'The import failed without a reason given.'} Nothing was
            published, so no instance can launch from this image until an import succeeds.
          </Alert>
        )}

        {isLoading && (
          <Box textAlign="center" padding="l">
            <Spinner size="large" />
          </Box>
        )}

        {image !== undefined && (
          <Container header={<Header variant="h2">Details</Header>}>
            <Grid gridDefinition={TWO_COLUMNS}>
              <Row label="Image ID">
                <Box variant="code">{image.id}</Box>
              </Row>
              <Row label="Architecture">
                {image.architecture === 'aarch64' ? 'arm64 (aarch64)' : (image.architecture ?? '—')}
              </Row>
              <Row label="Operating system">
                {[image.os_family, image.os_version]
                  .filter((part) => part !== undefined)
                  .join(' ') || '—'}
              </Row>
              <Row label="Default user">{image.default_user ?? '—'}</Row>
              <Row label="Format">{image.format ?? '—'}</Row>
              <Row label="Size on disk">
                {image.size_bytes === undefined || image.size_bytes === null
                  ? '—'
                  : formatBytes(image.size_bytes)}
              </Row>
              <Row label="Minimum disk size">
                {image.min_disk_gb === undefined ? '—' : `${image.min_disk_gb} GiB`}
              </Row>
              <Row label="Source URL">
                {image.source_url === undefined || image.source_url === null ? (
                  '—'
                ) : (
                  <Box variant="code">{image.source_url}</Box>
                )}
              </Row>
              <Row label="Checksum">
                {image.checksum === undefined || image.checksum === null ? (
                  'Not verified'
                ) : (
                  <Box variant="code">
                    {image.checksum.algorithm}: {image.checksum.value}
                  </Box>
                )}
              </Row>
              <Row label="Created">
                <Time value={image.created_at} />
              </Row>
              <Row label="Last updated">
                <Time value={image.updated_at} />
              </Row>
            </Grid>
          </Container>
        )}

        {tags.length > 0 && (
          <Container header={<Header variant="h2">Tags</Header>}>
            <Grid gridDefinition={TWO_COLUMNS}>
              {tags.map(([key, value]) => (
                <Row key={key} label={key}>
                  {value}
                </Row>
              ))}
            </Grid>
          </Container>
        )}

        <Container header={<Header variant="h2">Activity</Header>}>
          <SpaceBetween size="s">
            {tasks.length === 0 ? (
              <Box variant="p" color="text-status-inactive">
                No task has touched this image.
              </Box>
            ) : (
              tasks.map((task) => <TaskRow key={task.id} task={task} />)
            )}
          </SpaceBetween>
        </Container>
      </SpaceBetween>

      {isDeleteOpen && image !== undefined && (
        <Modal
          visible
          onDismiss={() => setDeleteOpen(false)}
          header={`Delete ${image.name}?`}
          closeAriaLabel="Close"
          footer={
            <Box float="right">
              <SpaceBetween direction="horizontal" size="s">
                <Button variant="link" onClick={() => setDeleteOpen(false)}>
                  Cancel
                </Button>
                <Button
                  variant="primary"
                  loading={isDeleting}
                  onClick={() => void onDelete()}
                  data-testid="confirm-delete-image"
                >
                  Delete
                </Button>
              </SpaceBetween>
            </Box>
          }
        >
          <SpaceBetween size="m">
            <Alert type="warning" header="This cannot be undone">
              Deleting an image removes the downloaded copy from this node. Instances that already
              launched keep their own root disk, but no new instance can launch from this image.
            </Alert>
            <Box variant="p">
              An image an instance still references cannot be deleted; terminate the instance first.
            </Box>
          </SpaceBetween>
        </Modal>
      )}
    </ContentLayout>
  )
}

/** TWO_COLUMNS puts two label/value pairs side by side on a wide screen. */
const TWO_COLUMNS: ReadonlyArray<GridProps.ElementDefinition> = [
  { colspan: { default: 6, m: 3 } },
  { colspan: { default: 6, m: 9 } },
]

/**
 * Row is one label/value line of a details container: the label in muted text,
 * the value below it. A value can be long (a source URL, a checksum), so the text
 * wraps instead of forcing the grid wider than the page.
 */
function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <SpaceBetween size="xxs">
      <Box variant="awsui-key-label">{label}</Box>
      <Box>{children}</Box>
    </SpaceBetween>
  )
}

/** Time renders a timestamp as relative text with the absolute time as its title. */
function Time({ value }: { value: string }) {
  return <span title={formatAbsoluteTime(value)}>{formatRelativeTime(value)}</span>
}

/**
 * TaskRow is one task in the activity list: what it was, how it ended, and when it
 * started. A task that never ran is shown as queued rather than as nothing,
 * because "pending" is the answer to "why has my image not moved?".
 */
function TaskRow({ task }: { task: Task }) {
  return (
    <SpaceBetween size="xs">
      <SpaceBetween size="s">
        <Box variant="code">{task.type}</Box>
        <StatusIndicator type={taskIndicatorType(task.state)}>
          {taskLabel(task.state)}
        </StatusIndicator>
      </SpaceBetween>

      <Box variant="small" color="text-status-inactive">
        {task.error ?? `queued ${formatRelativeTime(task.created_at)}`}
      </Box>
    </SpaceBetween>
  )
}

/** taskIndicatorType maps a task state onto a StatusIndicator type. */
function taskIndicatorType(state: Task['state']): 'pending' | 'in-progress' | 'success' | 'error' {
  switch (state) {
    case 'pending':
      return 'pending'
    case 'running':
      return 'in-progress'
    case 'succeeded':
      return 'success'
    case 'failed':
      return 'error'
  }
}

/** taskLabel is the human name of a task state. */
function taskLabel(state: Task['state']): string {
  switch (state) {
    case 'pending':
      return 'Queued'
    case 'running':
      return 'Running'
    case 'succeeded':
      return 'Succeeded'
    case 'failed':
      return 'Failed'
  }
}
