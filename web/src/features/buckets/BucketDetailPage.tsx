import { useMemo, useState, type ReactNode } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import Button from '@cloudscape-design/components/button'
import ColumnLayout from '@cloudscape-design/components/column-layout'
import Container from '@cloudscape-design/components/container'
import ContentLayout from '@cloudscape-design/components/content-layout'
import CopyToClipboard from '@cloudscape-design/components/copy-to-clipboard'
import FormField from '@cloudscape-design/components/form-field'
import Header from '@cloudscape-design/components/header'
import SpaceBetween from '@cloudscape-design/components/space-between'
import Spinner from '@cloudscape-design/components/spinner'
import StatusIndicator from '@cloudscape-design/components/status-indicator'
import TagEditor, { type TagEditorProps } from '@cloudscape-design/components/tag-editor'
import Tabs from '@cloudscape-design/components/tabs'

import { usePageMeta } from '@/app/pageMeta'
import { useAppDispatch, useAppSelector } from '@/app/store'
import { selectIsAdmin } from '@/features/auth/authSlice'
import {
  bucketResourceId,
  bucketsApi,
  pollInterval,
  useGetBucketQuery,
  useUpdateBucketMutation,
} from '@/features/buckets/api'
import { DeleteBucketModal } from '@/features/buckets/DeleteBucketModal'
import { ObjectBrowser } from '@/features/buckets/ObjectBrowser'
import { ObjectStorageGate } from '@/features/buckets/ObjectStorageGate'
import { BUCKET_INDICATOR, VERSIONING_LABEL } from '@/features/buckets/state'
import {
  pollInterval as taskPollInterval,
  tasksApi,
  useListTasksQuery,
  type Task,
} from '@/features/tasks/api'
import { taskIndicatorType, taskLabel } from '@/features/tasks/state'
import { notify } from '@/features/notifications/notificationsSlice'
import { errorMessage, fieldErrors, isNotFound, type ApiError } from '@/lib/apiError'
import { formatAbsoluteTime, formatRelativeTime } from '@/lib/format'

/** BucketDetailPage is one bucket's page, behind the object storage gate. */
export function BucketDetailPage() {
  const { bucketName } = useParams<{ bucketName: string }>()
  const name = bucketName ?? ''

  usePageMeta({
    breadcrumbs: [
      { text: 'Storage', href: '/buckets' },
      { text: 'Buckets', href: '/buckets' },
      { text: name, href: `/buckets/${encodeURIComponent(name)}` },
    ],
  })

  if (name === '') {
    return null
  }
  return (
    <ObjectStorageGate title={name}>
      <BucketDetail name={name} />
    </ObjectStorageGate>
  )
}

/**
 * BucketDetail shows a bucket in two tabs: Objects, the folder browser, and
 * Properties: its state and versioning, how an S3 client connects to it, its
 * tags and the tasks that worked on it.
 *
 * Versioning is a toggle and not a form field, because S3 has no way back to
 * "never versioned": turning it off suspends it, and the button says so.
 */
function BucketDetail({ name }: { name: string }) {
  const dispatch = useAppDispatch()
  const navigate = useNavigate()
  const isAdmin = useAppSelector(selectIsAdmin)

  const cached = useAppSelector(bucketsApi.endpoints.getBucket.select(name))
  const taskArgs = useMemo(() => ({ resourceId: bucketResourceId(name) }), [name])
  const cachedTasks = useAppSelector(tasksApi.endpoints.listTasks.select(taskArgs))

  const {
    data: bucket,
    isLoading,
    isFetching,
    error,
    refetch,
  } = useGetBucketQuery(name, {
    pollingInterval: pollInterval(cached.data === undefined ? undefined : [cached.data]),
  })
  const { data: tasksData } = useListTasksQuery(taskArgs, {
    pollingInterval: taskPollInterval(cachedTasks?.data?.items),
  })
  const [updateBucket, { isLoading: isUpdating }] = useUpdateBucketMutation()

  const [isDeleteOpen, setDeleteOpen] = useState(false)
  const [actionError, setActionError] = useState<ApiError>(undefined)
  const [isEditing, setIsEditing] = useState(false)
  const [editTags, setEditTags] = useState<ReadonlyArray<TagEditorProps.Tag>>([])
  const [tab, setTab] = useState('objects')

  const tasks = tasksData?.items ?? []
  const tags = Object.entries(bucket?.tags ?? {})
  const deleting = bucket?.state === 'deleting'
  const canWrite = isAdmin && bucket !== undefined && !deleting

  async function setVersioning(enabled: boolean) {
    setActionError(undefined)
    try {
      await updateBucket({ name, versioning: enabled }).unwrap()
      dispatch(
        notify({
          type: 'success',
          header: enabled ? 'Versioning enabled' : 'Versioning suspended',
          content: name,
        }),
      )
    } catch (err) {
      setActionError(err as ApiError)
    }
  }

  function beginEditing() {
    setEditTags(tags.map(([key, value]) => ({ key, value, existing: true })))
    setIsEditing(true)
  }

  async function saveTags() {
    setActionError(undefined)
    try {
      await updateBucket({
        name,
        tags: Object.fromEntries(
          editTags.filter((tag) => !tag.markedForRemoval).map((tag) => [tag.key, tag.value]),
        ),
      }).unwrap()
      setIsEditing(false)
      dispatch(notify({ type: 'success', header: 'Tags updated', content: name }))
    } catch (err) {
      setActionError(err as ApiError)
    }
  }

  if (isNotFound(error)) {
    return (
      <ContentLayout header={<Header variant="h1">{name}</Header>}>
        <Alert
          type="info"
          header="This bucket does not exist"
          action={<Button onClick={() => navigate('/buckets')}>Back to buckets</Button>}
        >
          It may have been deleted, by Kumo or by another S3 client.
        </Alert>
      </ContentLayout>
    )
  }

  const versioning = bucket?.versioning
  const cliExample = bucket
    ? `aws --endpoint-url ${bucket.endpoint} s3 ls s3://${bucket.name}/`
    : ''

  return (
    <ContentLayout
      header={
        <Header
          variant="h1"
          actions={
            <SpaceBetween direction="horizontal" size="s">
              <Button
                iconName="refresh"
                ariaLabel="Refresh bucket"
                loading={isFetching}
                onClick={() => void refetch()}
              />
              <Button
                disabled={!canWrite}
                onClick={() => setDeleteOpen(true)}
                data-testid="delete-bucket"
              >
                Delete
              </Button>
            </SpaceBetween>
          }
        >
          {name}
        </Header>
      }
    >
      <SpaceBetween size="l">
        {error !== undefined && (
          <Alert type="error" header="The bucket could not be loaded">
            {errorMessage(error, 'The bucket could not be loaded.')}
          </Alert>
        )}
        {actionError !== undefined && Object.keys(fieldErrors(actionError)).length === 0 && (
          <Alert type="error" header="The change was refused">
            {errorMessage(actionError, 'The bucket could not be changed.')}
          </Alert>
        )}
        {deleting && (
          <Alert type="info" header="This bucket is being deleted">
            A background task is removing every object and version, then the bucket. If the task
            fails, delete the bucket again with its contents to retry.
          </Alert>
        )}

        {isLoading && (
          <Box textAlign="center" padding="l">
            <Spinner size="large" />
          </Box>
        )}

        {bucket !== undefined && (
          <Tabs
            activeTabId={tab}
            onChange={({ detail }) => setTab(detail.activeTabId)}
            ariaLabel="Bucket sections"
            tabs={[
              {
                id: 'objects',
                label: 'Objects',
                content: <ObjectBrowser bucket={name} canWrite={canWrite} />,
              },
              {
                id: 'properties',
                label: 'Properties',
                content: (
                  <SpaceBetween size="l">
                    {
                      <Container header={<Header variant="h2">Details</Header>}>
                        <ColumnLayout columns={2} variant="text-grid">
                          <Row label="State">
                            <StatusIndicator type={BUCKET_INDICATOR[bucket.state].type}>
                              {BUCKET_INDICATOR[bucket.state].label}
                            </StatusIndicator>
                          </Row>
                          <Row label="Created">
                            <span title={formatAbsoluteTime(bucket.created_at)}>
                              {formatRelativeTime(bucket.created_at)}
                            </span>
                          </Row>
                          <Row label="Created by">{bucket.created_by ?? 'Outside Kumo'}</Row>
                          <Row label="Versioning">
                            <SpaceBetween direction="horizontal" size="s" alignItems="center">
                              <span data-testid="bucket-versioning">
                                {versioning === undefined ? '—' : VERSIONING_LABEL[versioning]}
                              </span>
                              {versioning === 'enabled' ? (
                                <Button
                                  variant="inline-link"
                                  disabled={!canWrite}
                                  loading={isUpdating}
                                  onClick={() => void setVersioning(false)}
                                >
                                  Suspend
                                </Button>
                              ) : (
                                <Button
                                  variant="inline-link"
                                  disabled={!canWrite}
                                  loading={isUpdating}
                                  onClick={() => void setVersioning(true)}
                                >
                                  Enable
                                </Button>
                              )}
                            </SpaceBetween>
                          </Row>
                        </ColumnLayout>
                      </Container>
                    }

                    {
                      <Container
                        header={
                          <Header
                            variant="h2"
                            description="Point any S3 client at this endpoint with path-style addressing and an access key."
                          >
                            Connect
                          </Header>
                        }
                      >
                        <ColumnLayout columns={3} variant="text-grid">
                          <Row label="Endpoint">
                            <CopyToClipboard
                              variant="inline"
                              textToCopy={bucket.endpoint}
                              copyButtonAriaLabel="Copy endpoint"
                              copySuccessText="Endpoint copied"
                              copyErrorText="Endpoint not copied"
                            />
                          </Row>
                          <Row label="Region">{bucket.region}</Row>
                          <Row label="Bucket">
                            <Box variant="code">{bucket.name}</Box>
                          </Row>
                        </ColumnLayout>
                        <Box margin={{ top: 'm' }}>
                          <Box variant="awsui-key-label">Example</Box>
                          <CopyToClipboard
                            variant="inline"
                            textToCopy={cliExample}
                            copyButtonAriaLabel="Copy example command"
                            copySuccessText="Command copied"
                            copyErrorText="Command not copied"
                          />
                        </Box>
                      </Container>
                    }

                    {
                      <Container
                        header={
                          <Header
                            variant="h2"
                            description="Key/value pairs to organize buckets. They are Kumo's, not S3 object tags."
                            actions={
                              isEditing ? (
                                <SpaceBetween direction="horizontal" size="s">
                                  <Button variant="link" onClick={() => setIsEditing(false)}>
                                    Cancel
                                  </Button>
                                  <Button
                                    variant="primary"
                                    loading={isUpdating}
                                    onClick={() => void saveTags()}
                                  >
                                    Save
                                  </Button>
                                </SpaceBetween>
                              ) : (
                                <Button disabled={!canWrite} onClick={beginEditing}>
                                  Edit tags
                                </Button>
                              )
                            }
                          >
                            Tags
                          </Header>
                        }
                      >
                        {isEditing ? (
                          <FormField label="Tags" errorText={fieldErrors(actionError)['tags']}>
                            <TagEditor
                              tags={editTags}
                              onChange={({ detail }) => setEditTags(detail.tags)}
                              i18nStrings={{ keyPlaceholder: 'Enter a tag key' }}
                              tagLimit={50}
                            />
                          </FormField>
                        ) : tags.length === 0 ? (
                          <Box variant="p" color="text-status-inactive">
                            This bucket has no tags.
                          </Box>
                        ) : (
                          <ColumnLayout columns={2} variant="text-grid">
                            {tags.map(([key, value]) => (
                              <Row key={key} label={key}>
                                {value === '' ? '—' : value}
                              </Row>
                            ))}
                          </ColumnLayout>
                        )}
                      </Container>
                    }

                    {
                      <Container header={<Header variant="h2">Activity</Header>}>
                        <SpaceBetween size="s">
                          {tasks.length === 0 ? (
                            <Box variant="p" color="text-status-inactive">
                              No task has touched this bucket.
                            </Box>
                          ) : (
                            tasks.map((task) => <TaskRow key={task.id} task={task} />)
                          )}
                        </SpaceBetween>
                      </Container>
                    }
                  </SpaceBetween>
                ),
              },
            ]}
          />
        )}
      </SpaceBetween>

      {isDeleteOpen && (
        <DeleteBucketModal
          name={name}
          onClose={() => setDeleteOpen(false)}
          onDeleted={() => navigate('/buckets')}
        />
      )}
    </ContentLayout>
  )
}

/** Row is one label/value line of a details container. */
function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <SpaceBetween size="xxs">
      <Box variant="awsui-key-label">{label}</Box>
      <Box>{children}</Box>
    </SpaceBetween>
  )
}

/** TaskRow is one task in the activity list: what it was and how it ended. */
function TaskRow({ task }: { task: Task }) {
  return (
    <SpaceBetween size="xs">
      <SpaceBetween direction="horizontal" size="s">
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
