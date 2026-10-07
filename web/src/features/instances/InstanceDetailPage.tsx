import { useMemo, useState, type ReactNode } from 'react'
import { useParams } from 'react-router-dom'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import Button from '@cloudscape-design/components/button'
import ButtonDropdown from '@cloudscape-design/components/button-dropdown'
import Container from '@cloudscape-design/components/container'
import ContentLayout from '@cloudscape-design/components/content-layout'
import FormField from '@cloudscape-design/components/form-field'
import ColumnLayout from '@cloudscape-design/components/column-layout'
import Header from '@cloudscape-design/components/header'
import Input from '@cloudscape-design/components/input'
import Link from '@cloudscape-design/components/link'
import Modal from '@cloudscape-design/components/modal'
import SpaceBetween from '@cloudscape-design/components/space-between'
import Spinner from '@cloudscape-design/components/spinner'
import StatusIndicator from '@cloudscape-design/components/status-indicator'
import Tabs from '@cloudscape-design/components/tabs'
import TagEditor, { type TagEditorProps } from '@cloudscape-design/components/tag-editor'

import { usePageMeta } from '@/app/pageMeta'
import { useAppDispatch, useAppSelector } from '@/app/store'
import {
  ACTIONS,
  ACTIONS_NEEDING_CONFIRMATION,
  ACTION_LABELS,
  canPerform,
  isForce,
  type InstanceAction,
} from '@/features/instances/actions'
import {
  instancesApi,
  pollInterval,
  useGetInstanceConsoleOutputQuery,
  useGetInstanceQuery,
  useRebootInstanceMutation,
  useStartInstanceMutation,
  useStopInstanceMutation,
  useTerminateInstanceMutation,
  useUpdateInstanceMutation,
} from '@/features/instances/api'
import { InstanceStateIndicator } from '@/features/instances/InstanceStateIndicator'
import { STATE_INDICATOR } from '@/features/instances/state'
import { notify } from '@/features/notifications/notificationsSlice'
import { pollInterval as taskPollInterval, tasksApi, useListTasksQuery } from '@/features/tasks/api'
import { errorMessage, fieldErrors, type ApiError } from '@/lib/apiError'
import { formatAbsoluteTime, formatGiB, formatRelativeTime } from '@/lib/format'

/** CONSOLE_TAIL_KIB is how much of the serial log the console tab asks for. */
const CONSOLE_TAIL_KIB = 64

/** An action waiting on its confirmation. */
interface PendingAction {
  action: InstanceAction
  typedName: string
}

/**
 * InstanceDetailPage shows one instance, its state, its storage, its tags, the
 * tail of its serial console and the tasks that worked on it
 * (03-frontend.md §6.3).
 *
 * The page polls while the instance is moving — 5 s while it is launching,
 * stopping or terminating, 30 s once it has settled (§5). Booting a guest is
 * measured in tens of seconds and a graceful stop in whatever the guest takes to
 * honour ACPI, so a page that did not watch would spend most of its life showing
 * a state that happened to be true when it loaded.
 *
 * The lifecycle actions live here too, not only on the list. An operator who
 * followed a launch to this page is watching an instance come up, and the thing
 * they want when a boot hangs is Stop — three clicks and a page change away from
 * where they already are.
 *
 * `user_data` is shown because this page is administrator-only for everything else
 * already: the endpoint only carries it for an admin (01-api.md §Instances), and
 * the console never puts it anywhere but this component's state.
 */
export function InstanceDetailPage() {
  const { instanceId } = useParams<{ instanceId: string }>()
  const dispatch = useAppDispatch()

  const id = instanceId ?? ''
  // Memoized so the subscription keys stay stable across renders.
  const taskArgs = useMemo(() => ({ resourceId: id }), [id])
  const consoleArgs = useMemo(() => ({ id, tailKib: CONSOLE_TAIL_KIB }), [id])
  // The cached copies are read with selectors because a poll interval has to be
  // chosen before the subscription starts, and the data cannot come from the very
  // call that sets it up. Reading the cache fetches nothing.
  const cachedInstance = useAppSelector(instancesApi.endpoints.getInstance.select(id))
  const cachedTasks = useAppSelector(tasksApi.endpoints.listTasks.select(taskArgs))

  const {
    data: instance,
    isLoading,
    isFetching,
    error,
    refetch,
  } = useGetInstanceQuery(id, {
    skip: id === '',
    pollingInterval: pollInterval(
      cachedInstance?.data === undefined ? undefined : [cachedInstance.data],
    ),
  })
  const { data: tasksData } = useListTasksQuery(taskArgs, {
    skip: id === '',
    pollingInterval: taskPollInterval(cachedTasks?.data?.items),
  })
  const {
    data: consoleOutput,
    isFetching: isFetchingConsole,
    refetch: refetchConsole,
  } = useGetInstanceConsoleOutputQuery(consoleArgs, { skip: id === '' })

  const [startInstance] = useStartInstanceMutation()
  const [stopInstance] = useStopInstanceMutation()
  const [rebootInstance] = useRebootInstanceMutation()
  const [terminateInstance] = useTerminateInstanceMutation()
  const [updateInstance, { isLoading: isSavingTags }] = useUpdateInstanceMutation()

  const [pending, setPending] = useState<PendingAction | null>(null)
  const [actionError, setActionError] = useState<ApiError>(undefined)
  const [isEditing, setIsEditing] = useState(false)
  const [editTags, setEditTags] = useState<ReadonlyArray<TagEditorProps.Tag>>([])

  usePageMeta({
    breadcrumbs: [
      { text: 'Compute', href: '/' },
      { text: 'Instances', href: '/instances' },
      { text: instance?.name ?? id, href: `/instances/${id}` },
    ],
  })

  const tasks = tasksData?.items ?? []
  const tags = Object.entries(instance?.tags ?? {})
  const canAct = instance !== undefined && !isFetching

  /** run fires an action that needs no confirmation. */
  async function run(action: InstanceAction) {
    if (instance === undefined) {
      return
    }
    setActionError(undefined)
    try {
      switch (action) {
        case 'start':
          await startInstance(instance.id).unwrap()
          break
        case 'stop':
        case 'force-stop':
          await stopInstance({ id: instance.id, force: isForce(action) }).unwrap()
          break
        case 'reboot':
          await rebootInstance({ id: instance.id, force: false }).unwrap()
          break
        case 'terminate':
          await terminateInstance(instance.id).unwrap()
          break
      }
      dispatch(
        notify({
          type: 'success',
          header: `${ACTION_LABELS[action]} started`,
          content: `${instance.name}: ${ACTION_LABELS[action].toLowerCase()} is running in the background.`,
        }),
      )
    } catch (err) {
      setActionError(err as ApiError)
    }
  }

  /** onAction routes through the confirmation when the action needs one. */
  function onAction(action: InstanceAction) {
    if (instance === undefined) {
      return
    }
    setActionError(undefined)
    if (ACTIONS_NEEDING_CONFIRMATION.has(action)) {
      setPending({ action, typedName: '' })
      return
    }
    void run(action)
  }

  /** beginEditing seeds the tag editor from what the instance carries now. */
  function beginEditing() {
    setEditTags(tags.map(([key, value]) => ({ key, value, existing: true })))
    setIsEditing(true)
  }

  async function saveTags() {
    if (instance === undefined) {
      return
    }
    setActionError(undefined)
    try {
      await updateInstance({
        id: instance.id,
        tags: Object.fromEntries(editTags.map((tag) => [tag.key, tag.value])),
      }).unwrap()
      setIsEditing(false)
      dispatch(notify({ type: 'success', header: 'Tags updated', content: instance.name }))
    } catch (err) {
      setActionError(err as ApiError)
    }
  }

  // Only terminate demands the name. Force stop destroys work but not the
  // instance, so asking for a typed name there would leave the button shut
  // forever, because that dialog has no field to fill it from.
  const pendingNameMatches =
    pending !== null &&
    (pending.action !== 'terminate' || pending.typedName === (instance?.name ?? ''))

  return (
    <ContentLayout
      header={
        <Header
          variant="h1"
          // The name is the heading, but the id is what an operator pastes into a
          // `virsh` command, so it is selectable text rather than a tooltip.
          description={<Box variant="code">{id}</Box>}
          actions={
            <SpaceBetween direction="horizontal" size="s">
              <Button
                iconName="refresh"
                ariaLabel="Refresh instance"
                loading={isFetching}
                onClick={() => void refetch()}
              />
              <ButtonDropdown
                disabled={!canAct}
                items={ACTIONS.map((action) => ({
                  id: action,
                  text: ACTION_LABELS[action],
                  // Disabled rather than hidden: an operator watching a failed
                  // instance should be able to see that Start exists and why it is
                  // not the thing they can press right now.
                  disabled: instance !== undefined && !canPerform(action, instance.state),
                }))}
                onItemClick={({ detail }) => onAction(detail.id as InstanceAction)}
              >
                Instance state
              </ButtonDropdown>
            </SpaceBetween>
          }
        >
          {instance?.name ?? 'Instance'}
        </Header>
      }
    >
      <SpaceBetween size="l">
        {error !== undefined && (
          <Alert type="error" header="The instance could not be loaded">
            {errorMessage(error, 'The instance could not be loaded.')}
          </Alert>
        )}

        {isLoading && (
          <Box textAlign="center" padding="l">
            <Spinner size="large" />
          </Box>
        )}

        {instance?.state === 'error' && (
          <Alert type="error" header="The instance is in an error state">
            {instance.state_reason ?? 'The node reported an error without giving a reason.'} The
            console log below has the guest's own view of what happened.
          </Alert>
        )}

        {instance !== undefined && <InstanceStateIndicator instance={instance} />}

        {actionError !== undefined && (
          <Alert type="error" header="The action failed">
            {errorMessage(actionError, 'The action could not be started.')}
          </Alert>
        )}

        {instance !== undefined && (
          <Tabs
            tabs={[
              {
                id: 'details',
                label: 'Details',
                content: (
                  <Container header={<Header variant="h2">Details</Header>}>
                    <ColumnLayout columns={2} variant="text-grid">
                      <Row label="Instance ID">
                        <Box variant="code">{instance.id}</Box>
                      </Row>
                      <Row label="State">
                        <StatusIndicator type={STATE_INDICATOR[instance.state].type}>
                          {STATE_INDICATOR[instance.state].label}
                        </StatusIndicator>
                      </Row>
                      <Row label="Instance type">{instance.instance_type}</Row>
                      <Row label="vCPUs">{instance.vcpus}</Row>
                      <Row label="Memory">{formatGiB(Math.round(instance.memory_mib / 1024))}</Row>
                      <Row label="Image">
                        <Link href={`/images/${instance.image_id}`}>
                          {instance.image_name ?? instance.image_id}
                        </Link>
                      </Row>
                      <Row label="Key pair">
                        {instance.key_pair_name === null ? (
                          '—'
                        ) : (
                          <Link href={`/key-pairs`}>{instance.key_pair_name}</Link>
                        )}
                      </Row>
                      <Row label="Private IP">{instance.private_ip ?? '—'}</Row>
                      <Row label="MAC address">
                        {instance.mac_address === null ? (
                          '—'
                        ) : (
                          <Box variant="code">{instance.mac_address}</Box>
                        )}
                      </Row>
                      <Row label="VPC">
                        {instance.vpc_id === null ? (
                          '—'
                        ) : (
                          <Link href={`/vpcs/${instance.vpc_id}`}>{instance.vpc_id}</Link>
                        )}
                      </Row>
                      <Row label="Subnet">
                        {instance.subnet_id === null ? (
                          '—'
                        ) : (
                          <Link href={`/subnets/${instance.subnet_id}`}>{instance.subnet_id}</Link>
                        )}
                      </Row>
                      <Row label="Launched">
                        <Time value={instance.launched_at} />
                      </Row>
                      <Row label="Last updated">
                        {instance.updated_at === undefined ? (
                          '—'
                        ) : (
                          <Time value={instance.updated_at} />
                        )}
                      </Row>
                      {instance.terminated_at !== undefined && instance.terminated_at !== null && (
                        <Row label="Terminated">
                          <Time value={instance.terminated_at} />
                        </Row>
                      )}
                      <Row label="Task">
                        {instance.task_id === null ? (
                          '—'
                        ) : (
                          <Box variant="code">{instance.task_id}</Box>
                        )}
                      </Row>
                    </ColumnLayout>
                  </Container>
                ),
              },
              {
                id: 'storage',
                label: 'Storage',
                content: (
                  <Container header={<Header variant="h2">Storage</Header>}>
                    <ColumnLayout columns={2} variant="text-grid">
                      <Row label="Root disk">{formatGiB(instance.root_disk_gb)}</Row>
                      <Row label="Source image">
                        <Link href={`/images/${instance.image_id}`}>
                          {instance.image_name ?? instance.image_id}
                        </Link>
                      </Row>
                      <Row label="Resize">Not available yet</Row>
                    </ColumnLayout>
                    <Box variant="p" color="text-status-inactive">
                      Kumo copies the image into this disk, so the instance is independent of the
                      image: deleting the image later does not touch it. Resizing is not available
                      yet.
                    </Box>
                  </Container>
                ),
              },
              {
                id: 'tags',
                label: 'Tags',
                content: (
                  <Container
                    header={
                      <Header
                        variant="h2"
                        description="Key/value pairs to organize instances."
                        actions={
                          isEditing ? (
                            <SpaceBetween direction="horizontal" size="s">
                              <Button variant="link" onClick={() => setIsEditing(false)}>
                                Cancel
                              </Button>
                              <Button
                                variant="primary"
                                loading={isSavingTags}
                                onClick={() => void saveTags()}
                              >
                                Save
                              </Button>
                            </SpaceBetween>
                          ) : (
                            <Button onClick={beginEditing}>Edit tags</Button>
                          )
                        }
                      >
                        Tags
                      </Header>
                    }
                  >
                    {isEditing ? (
                      <FormField
                        label="Tags"
                        errorText={fieldErrors(actionError)['tags']}
                        description="Optional key/value pairs to organize instances."
                      >
                        <TagEditor
                          tags={editTags}
                          onChange={({ detail }) => setEditTags(detail.tags)}
                          i18nStrings={{ keyPlaceholder: 'Enter a tag key' }}
                          tagLimit={50}
                        />
                      </FormField>
                    ) : tags.length === 0 ? (
                      <Box variant="p" color="text-status-inactive">
                        This instance has no tags.
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
                ),
              },
              {
                id: 'console',
                label: 'Console output',
                content: (
                  <Container
                    header={
                      <Header
                        variant="h2"
                        description="The tail of the serial log."
                        actions={
                          <Button
                            iconName="refresh"
                            ariaLabel="Refresh console output"
                            loading={isFetchingConsole}
                            onClick={() => void refetchConsole()}
                          />
                        }
                      >
                        Console output
                      </Header>
                    }
                  >
                    <SpaceBetween size="s">
                      <Box variant="small" color="text-status-inactive">
                        The tail of the serial log, which is where a guest that never boots explains
                        itself. An interactive console is on the roadmap.
                      </Box>
                      {consoleOutput === undefined || consoleOutput.output === '' ? (
                        <Box variant="p" color="text-status-inactive">
                          The console has not written anything yet.
                        </Box>
                      ) : (
                        <pre
                          style={{ maxHeight: 480, overflow: 'auto', whiteSpace: 'pre-wrap' }}
                          data-testid="console-output"
                        >
                          {consoleOutput.output}
                        </pre>
                      )}
                    </SpaceBetween>
                  </Container>
                ),
              },
              {
                id: 'tasks',
                label: 'Tasks',
                content: (
                  <Container header={<Header variant="h2">Activity</Header>}>
                    {tasks.length === 0 ? (
                      <Box variant="p" color="text-status-inactive">
                        No task has touched this instance.
                      </Box>
                    ) : (
                      <SpaceBetween size="s">
                        {tasks.map((task) => (
                          <SpaceBetween key={task.id} size="xs">
                            <SpaceBetween direction="horizontal" size="s">
                              <Box variant="code">{task.type}</Box>
                              <Box variant="code">{task.id}</Box>
                            </SpaceBetween>
                            <Box variant="small" color="text-status-inactive">
                              {task.error ?? `queued ${formatRelativeTime(task.created_at)}`}
                            </Box>
                          </SpaceBetween>
                        ))}
                      </SpaceBetween>
                    )}
                  </Container>
                ),
              },
            ]}
            ariaLabel="Instance details"
          />
        )}
      </SpaceBetween>

      {pending !== null && instance !== undefined && (
        <Modal
          visible
          onDismiss={() => setPending(null)}
          header={
            pending.action === 'terminate'
              ? `Terminate ${instance.name}?`
              : `Force stop ${instance.name}?`
          }
          closeAriaLabel="Close"
          footer={
            <Box float="right">
              <SpaceBetween direction="horizontal" size="s">
                <Button variant="link" onClick={() => setPending(null)}>
                  Cancel
                </Button>
                <Button
                  variant="primary"
                  loading={isFetching}
                  // Terminate additionally requires the name, so a mis-click on the
                  // wrong instance cannot delete the wrong guest's disks.
                  disabled={!pendingNameMatches}
                  onClick={() => {
                    const action = pending.action
                    setPending(null)
                    void run(action)
                  }}
                  data-testid="confirm-destructive"
                >
                  {ACTION_LABELS[pending.action]}
                </Button>
              </SpaceBetween>
            </Box>
          }
        >
          <SpaceBetween size="m">
            <Alert type="warning" header="This cannot be undone">
              {pending.action === 'terminate'
                ? 'Terminating removes the domain and both of its disks. The name is kept as history so it can be reused, but nothing of the instance survives.'
                : 'A force stop cuts power to the guest. Anything it has not written to disk is lost, exactly as if the machine had been unplugged.'}
            </Alert>

            {pending.action === 'terminate' && (
              <FormField
                label="Type the name to confirm"
                description={`Enter "${instance.name}" to enable the button.`}
              >
                <Input
                  value={pending.typedName}
                  onChange={({ detail }) =>
                    setPending((current) =>
                      current === null ? null : { ...current, typedName: detail.value },
                    )
                  }
                  ariaLabel="Type the name to confirm"
                />
              </FormField>
            )}
          </SpaceBetween>
        </Modal>
      )}
    </ContentLayout>
  )
}

/**
 * Row is one label/value line of a details container: the label in muted text,
 * the value below it.
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
